// +build linux

package iouring

import (
	"errors"
	"log"
	"sync"
	"syscall"
	"unsafe"

	iouring_syscall "github.com/iceber/iouring-go/syscall"
)

// IOURing contains iouring_syscall submission and completion queue.
// It's safe for concurrent use by multiple goroutines.
type IOURing struct {
	params *iouring_syscall.IOURingParams
	fd     int

	sq *SubmissionQueue
	cq *CompletionQueue

	async bool
	Flags uint32

	submitLock sync.Mutex

	userDataLock sync.RWMutex
	userDatas    map[uint64]*UserData

	fileRegister FileRegister
}

// New return a IOURing instance by IOURingOptions
func New(entries uint, opts ...IOURingOption) (iour *IOURing, err error) {
	iour = &IOURing{
		params:    &iouring_syscall.IOURingParams{},
		userDatas: make(map[uint64]*UserData),
	}

	for _, opt := range opts {
		opt(iour)
	}

	iour.fd, err = iouring_syscall.IOURingSetup(entries, iour.params)
	if err != nil {
		log.Println("setup", err)
		return nil, err
	}

	if err := mmapIOURing(iour); err != nil {
		log.Println("mmap", err)
		return nil, err
	}

	iour.fileRegister = &fileRegister{
		iouringFd:    iour.fd,
		indexs:       make(map[int32]int),
		sparseIndexs: make(map[int]int),
	}
	iour.Flags = iour.params.Flags

	go iour.run()
	return iour, nil
}

// TODO(iceber): get available entry use async notification
func (iour *IOURing) getSQEntry() *iouring_syscall.SubmissionQueueEntry {
	for {
		sqe := iour.sq.GetSQEntry()
		if sqe != nil {
			return sqe
		}
	}
}

// SubmitRequest by IORequest function and io result is notified via channel
// return request id, can be used to cancel a request
func (iour *IOURing) SubmitRequest(request IORequest, ch chan<- *Result) (uint64, error) {
	iour.submitLock.Lock()
	defer iour.submitLock.Unlock()

	sqe := iour.getSQEntry()
	id, err := iour.doRequest(sqe, request, ch)
	if err != nil {
		iour.sq.fallback(1)
		return id, err
	}

	_, err = iour.submit()
	return id, err
}

// SubmitRequests by IORequest functions and io results are notified via channel
func (iour *IOURing) SubmitRequests(requests []IORequest, ch chan<- *Result) error {
	// TODO(iceber): no length limit
	if len(requests) > int(*iour.sq.entries) {
		return errors.New("requests is too many")
	}

	iour.submitLock.Lock()
	defer iour.submitLock.Unlock()

	var sqeN uint32
	for _, request := range requests {
		sqe := iour.getSQEntry()
		sqeN++

		if _, err := iour.doRequest(sqe, request, ch); err != nil {
			iour.sq.fallback(sqeN)
			return err
		}
	}
	_, err := iour.submit()
	return err
}

// CancelRequest by request id
func (iour *IOURing) CancelRequest(id uint64, ch chan<- *Result) error {
	_, err := iour.SubmitRequest(cancel(id), ch)
	return err
}

func (iour *IOURing) needEnter(flags *uint32) bool {
	if (iour.Flags & iouring_syscall.IORING_SETUP_FLAGS_SQPOLL) == 0 {
		return true
	}

	if iour.sq.needWakeup() {
		*flags |= iouring_syscall.IORING_SQ_NEED_WAKEUP
		return true
	}
	return false
}

func (iour *IOURing) submitAndWait(waitCount uint32) (submitted int, err error) {
	submitted = iour.sq.flush()

	var flags uint32
	if !iour.needEnter(&flags) && waitCount == 0 {
		return
	}

	if waitCount != 0 || (iour.Flags&iouring_syscall.IORING_SETUP_FLAGS_IOPOLL) != 0 {
		flags |= iouring_syscall.IORING_ENTER_FLAGS_GETEVENTS
	}

	submitted, err = iouring_syscall.IOURingEnter(iour.fd, uint32(submitted), waitCount, flags, nil)
	return
}

func (iour *IOURing) submit() (submitted int, err error) {
	submitted = iour.sq.flush()

	var flags uint32
	if !iour.needEnter(&flags) || submitted == 0 {
		return
	}

	if (iour.Flags & iouring_syscall.IORING_SETUP_FLAGS_IOPOLL) != 0 {
		flags |= iouring_syscall.IORING_ENTER_FLAGS_GETEVENTS
	}

	submitted, err = iouring_syscall.IOURingEnter(iour.fd, uint32(submitted), 0, flags, nil)
	return
}

func (iour *IOURing) doRequest(sqe *iouring_syscall.SubmissionQueueEntry, request IORequest, ch chan<- *Result) (id uint64, err error) {
	userData := makeUserData(ch)

	request(sqe, userData)
	userData.setOpcode(sqe.Opcode())

	id = uint64(uintptr(unsafe.Pointer(userData)))
	iour.userDatas[id] = userData
	sqe.SetUserData(id)

	if sqe.Fd() >= 0 {
		if index, ok := iour.fileRegister.GetFileIndex(int32(sqe.Fd())); ok {
			sqe.SetFdIndex(int32(index))
		} else if (iour.Flags & iouring_syscall.IORING_SETUP_FLAGS_SQPOLL) == 1 {
			return 0, errors.New("fd is not registered")
		}
	}

	if iour.async {
		sqe.SetFlags(iouring_syscall.IOSQE_FLAGS_ASYNC)
	}
	return
}

func (iour *IOURing) getCQEvent(wait bool) (cqe *iouring_syscall.CompletionQueueEvent, err error) {
	for {
		if cqe = iour.cq.peek(); cqe != nil {
			iour.cq.advance(1)
			return
		}

		if !wait && !iour.sq.cqOverflow() {
			err = syscall.EAGAIN
			return
		}

		_, err = iouring_syscall.IOURingEnter(iour.fd, 0, 1, iouring_syscall.IORING_ENTER_FLAGS_GETEVENTS, nil)
		if err != nil {
			return
		}
	}
}

func (iour *IOURing) run() {
	for {
		cqe, err := iour.getCQEvent(true)
		if cqe == nil || err != nil {
			log.Println("runComplete error: ", err)
			continue
		}

		log.Println("cqe user data", (cqe.UserData))

		userData := iour.userDatas[cqe.UserData]
		if userData == nil {
			log.Println("runComplete: notfound user data ", uintptr(cqe.UserData))
			continue
		}
		delete(iour.userDatas, cqe.UserData)
		userData.result.load(cqe)

		userData.done <- userData.result
	}
}

func cancel(id uint64) IORequest {
	return func(sqe *iouring_syscall.SubmissionQueueEntry, userData *UserData) {
		userData.result.resolver = cancelResolver
		sqe.PrepOperation(iouring_syscall.IORING_OP_ASYNC_CANCEL, -1, id, 0, 0)
	}
}