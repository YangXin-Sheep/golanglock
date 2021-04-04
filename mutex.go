// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package sync provides basic synchronization primitives such as mutual
// exclusion locks. Other than the Once and WaitGroup types, most are intended
// for use by low-level library routines. Higher-level synchronization is
// better done via channels and communication.
//
// Values containing the types defined in this package should not be copied.
package sync

import (
	"fmt"
	"internal/race"
	"sync/atomic"
	"unsafe"
)

func throw(string) // provided by runtime

// A Mutex is a mutual exclusion lock.
// The zero value for a Mutex is an unlocked mutex.
//
// A Mutex must not be copied after first use.
type Mutex struct {
	state int32
	sema  uint32
}

// A Locker represents an object that can be locked and unlocked.
type Locker interface {
	Lock()
	Unlock()
}

/*state    31 - 3		| 2 		| 1 				| 0
		   阻塞的协程数	 | 是否饥饿   | 是否有被唤醒的协程 | 是否在锁定状态
	锁工作在两种状态下: 1、正常(非饥饿)状态 2、饥饿状态  通过mutexStarving标记
	从正常状态进入饥饿状态前需要处理完所有被唤醒的协程,让协程全部进入阻塞睡眠后切换成饥饿状态

	正常状态: 随机抢占					//冲突不多
	饥饿状态: 按等待时间进行排队抢占	 //冲突多了不能随机了,否则某些协程卡死
	乐观锁不能瞎用,不考虑饥饿状态的乐观锁会出问题的!

 */
const (
	mutexLocked = 1 << iota // mutex is locked
	mutexWoken
	mutexStarving
	mutexWaiterShift = iota

	/*
		某个协程获取锁的时间超过1ms进入饥饿状态！等太久了 不能再随机抢占了
	*/
	starvationThresholdNs = 1e6
)


func (m *Mutex) Lock() {
	// 首次就抢占成功的直接返回
	if atomic.CompareAndSwapInt32(&m.state, 0, mutexLocked) {
		if race.Enabled {
			race.Acquire(unsafe.Pointer(m))
		}
		return
	}

	var waitStartTime int64
	starving := false
	awoke := false
	iter := 0
	old := m.state

	// 首次抢占失败的进入抢占循环
	for {
		// 只有正常模式的锁定状态 以及 自选次数不超过 4次的 协程才可以进入 自旋
		if old&(mutexLocked|mutexStarving) == mutexLocked && runtime_canSpin(iter) {
			// 进入自旋后 唤醒标记没有被 设置所有协程抢占设置唤醒标记
			// 进入饥饿状态后，所有协程都没办法 设置抢占标记啦
			if !awoke && old&mutexWoken == 0 && old>>mutexWaiterShift != 0 &&
				atomic.CompareAndSwapInt32(&m.state, old, old|mutexWoken) {
				awoke = true
			}
			runtime_doSpin()
			iter++
			old = m.state
			continue
		}
		new := old
		// 不是饥饿状态 当前协程可以尝试设置抢占位
		if old&mutexStarving == 0 {
			new |= mutexLocked
		}
		// 饥饿状态阻塞的协程计数加1
		if old&(mutexLocked|mutexStarving) != 0 {
			new += 1 << mutexWaiterShift
		}
		// 饥饿状态 并且 锁定 当前协程设置饥饿状态位
		if starving && old&mutexLocked != 0 {
			new |= mutexStarving
		}
		if awoke {
			// The goroutine has been woken from sleep,
			// so we need to reset the flag in either case.
			if new&mutexWoken == 0 {
				throw("sync: inconsistent mutex state")
			}
			// 说明当前协程抢到了唤醒标记，下面的操作可能使 当前协程阻塞，当前协程操作后需要让出唤醒标记哦
			new &^= mutexWoken
		}

		// 抢到唤醒标记的协程 设置成功 让出唤醒标记
		// 没有抢到唤醒标记的协程设置成功的话，说明当前并发状态下还是有被唤醒的进程在 run
		if atomic.CompareAndSwapInt32(&m.state, old, new) {

			// 正常模式 并且 未锁定，当前协程 抢锁成功
			// 如果是 拿不到锁 或者是饥饿状态 最终所有协程都要等待哈
			if old&(mutexLocked|mutexStarving) == 0 {
				break
			}
			// If we were already waiting before, queue at the front of the queue.
			queueLifo := waitStartTime != 0
			if waitStartTime == 0 {
				waitStartTime = runtime_nanotime()
			}

			// 饥饿 或者 没有抢到锁的 协程 睡眠 Zzz 
			runtime_SemacquireMutex(&m.sema, queueLifo)
			starving = starving || runtime_nanotime()-waitStartTime > starvationThresholdNs
			old = m.state
			if old&mutexStarving != 0 {
				if old&(mutexLocked|mutexWoken) != 0 || old>>mutexWaiterShift == 0 {
					throw("sync: inconsistent mutex state")
				}
				delta := int32(mutexLocked - 1<<mutexWaiterShift)
				// 解除饥饿状态 或者 自己是最后一个等待协程
				if !starving || old>>mutexWaiterShift == 1 {
					delta -= mutexStarving // 解除饥饿状态
				}
				atomic.AddInt32(&m.state, delta)
				break
			}
			awoke = true // unlock 唤醒后设置了 唤醒标记了，被唤醒的协程设置自己为唤醒状态啦
			iter = 0
		} else {
			old = m.state
		}
	}

	if race.Enabled {
		race.Acquire(unsafe.Pointer(m))
	}
}

func (m *Mutex) Unlock() {
	if race.Enabled {
		_ = m.state
		race.Release(unsafe.Pointer(m))
	}

	// Fast path: drop lock bit.
	new := atomic.AddInt32(&m.state, -mutexLocked)
	if (new+mutexLocked)&mutexLocked == 0 {
		throw("sync: unlock of unlocked mutex")
	}
	if new&mutexStarving == 0 {
		old := new
		for {
			// 如果没有等待的协程 或者  有锁定 唤醒协程runing 饥饿状态 任何一个条件成立    直接返回
			if old>>mutexWaiterShift == 0 || old&(mutexLocked|mutexWoken|mutexStarving) != 0 {
				return
			}
			// 如果存在等待的协程 | 设置唤醒标记 | 接触锁定
			new = (old - 1<<mutexWaiterShift) | mutexWoken
			if atomic.CompareAndSwapInt32(&m.state, old, new) {
				runtime_Semrelease(&m.sema, false) // 唤醒一个协程 因为新状态设置了唤醒标记
				return
			}
			old = m.state
		}
	} else {
		// 进入饥饿状态 不释放抢占标记哦, 所有正常状态下的协程最终在runtime_SemacquireMutex睡眠 Zzz
		runtime_Semrelease(&m.sema, true)
	}
}
