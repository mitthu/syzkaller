// Copyright 2017 syzkaller project authors. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package main

import (
	"bytes"
	"fmt"
	"math/rand"
	"os"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/google/syzkaller/pkg/cover"
	"github.com/google/syzkaller/pkg/hash"
	"github.com/google/syzkaller/pkg/ipc"
	. "github.com/google/syzkaller/pkg/log"
	. "github.com/google/syzkaller/pkg/rpctype"
	"github.com/google/syzkaller/prog"
)

const (
	programLength = 30
)

// Proc represents a single fuzzing process (executor).
type Proc struct {
	fuzzer *Fuzzer
	pid    int
	env    *ipc.Env
	rnd    *rand.Rand
}

func newProc(fuzzer *Fuzzer, pid int) (*Proc, error) {
	env, err := ipc.MakeEnv(fuzzer.config, pid)
	if err != nil {
		return nil, err
	}
	rnd := rand.New(rand.NewSource(time.Now().UnixNano() + int64(pid)*1e12))
	proc := &Proc{
		fuzzer: fuzzer,
		pid:    pid,
		env:    env,
		rnd:    rnd,
	}
	return proc, nil
}

func (proc *Proc) loop() {
	pid := proc.pid
	execOpts := proc.fuzzer.execOpts
	ct := proc.fuzzer.choiceTable

	for i := 0; ; i++ {
		item := proc.fuzzer.workQueue.dequeue()
		if item != nil {
			switch item := item.(type) {
			case *WorkTriage:
				proc.triageInput(item)
			case *WorkCandidate:
				proc.execute(execOpts, item.p, false, item.minimized,
					true, false, StatCandidate)
			case *WorkSmash:
				proc.smashInput(item)
			default:
				panic("unknown work type")
			}
			continue
		}

		corpus := proc.fuzzer.corpusSnapshot()
		if len(corpus) == 0 || i%100 == 0 {
			// Generate a new prog.
			p := target.Generate(proc.rnd, programLength, ct)
			Logf(1, "#%v: generated", pid)
			proc.execute(execOpts, p, false, false, false, false, StatGenerate)
		} else {
			// Mutate an existing prog.
			p := corpus[proc.rnd.Intn(len(corpus))].Clone()
			p.Mutate(proc.rnd, programLength, ct, corpus)
			Logf(1, "#%v: mutated", pid)
			proc.execute(execOpts, p, false, false, false, false, StatFuzz)
		}
	}
}

func (proc *Proc) triageInput(item *WorkTriage) {
	Logf(1, "#%v: triaging minimized=%v candidate=%v", proc.pid, item.minimized, item.candidate)

	execOpts := proc.fuzzer.execOpts
	if noCover {
		panic("should not be called when coverage is disabled")
	}

	signalMu.RLock()
	newSignal := cover.SignalDiff(corpusSignal, item.signal)
	signalMu.RUnlock()
	if len(newSignal) == 0 {
		return
	}
	newSignal = cover.Canonicalize(newSignal)

	call := item.p.Calls[item.call].Meta

	Logf(3, "triaging input for %v (new signal=%v)", call.CallName, len(newSignal))
	var inputCover cover.Cover
	opts := *execOpts
	opts.Flags |= ipc.FlagCollectCover
	opts.Flags &= ^ipc.FlagCollide
	if item.minimized {
		// We just need to get input coverage.
		for i := 0; i < 3; i++ {
			info := proc.executeRaw(&opts, item.p, StatTriage)
			if len(info) == 0 || len(info[item.call].Cover) == 0 {
				continue // The call was not executed. Happens sometimes.
			}
			inputCover = append([]uint32{}, info[item.call].Cover...)
			break
		}
	} else {
		// We need to compute input coverage and non-flaky signal for minimization.
		notexecuted := false
		for i := 0; i < 3; i++ {
			info := proc.executeRaw(&opts, item.p, StatTriage)
			if len(info) == 0 || len(info[item.call].Signal) == 0 {
				// The call was not executed. Happens sometimes.
				if notexecuted {
					return // if it happened twice, give up
				}
				notexecuted = true
				continue
			}
			inf := info[item.call]
			newSignal = cover.Intersection(newSignal, cover.Canonicalize(inf.Signal))
			if len(newSignal) == 0 {
				return
			}
			if len(inputCover) == 0 {
				inputCover = append([]uint32{}, inf.Cover...)
			} else {
				inputCover = cover.Union(inputCover, inf.Cover)
			}
		}

		item.p, item.call = prog.Minimize(item.p, item.call, func(p1 *prog.Prog, call1 int) bool {
			info := proc.execute(execOpts, p1, false, false, false, true, StatMinimize)
			if len(info) == 0 || len(info[call1].Signal) == 0 {
				return false // The call was not executed.
			}
			inf := info[call1]
			signal := cover.Canonicalize(inf.Signal)
			signalMu.RLock()
			defer signalMu.RUnlock()
			if len(cover.Intersection(newSignal, signal)) != len(newSignal) {
				return false
			}
			return true
		}, false)
	}

	data := item.p.Serialize()
	sig := hash.Hash(data)

	Logf(2, "added new input for %v to corpus:\n%s", call.CallName, data)
	a := &NewInputArgs{
		Name: *flagName,
		RpcInput: RpcInput{
			Call:   call.CallName,
			Prog:   data,
			Signal: []uint32(cover.Canonicalize(item.signal)),
			Cover:  []uint32(inputCover),
		},
	}
	if err := manager.Call("Manager.NewInput", a, nil); err != nil {
		panic(err)
	}

	signalMu.Lock()
	cover.SignalAdd(corpusSignal, item.signal)
	signalMu.Unlock()

	proc.fuzzer.addInputToCorpus(item.p, sig)

	if !item.minimized {
		proc.fuzzer.workQueue.enqueue(&WorkSmash{item.p, item.call})
	}
}

func (proc *Proc) smashInput(item *WorkSmash) {
	if faultInjectionEnabled {
		proc.failCall(item.p, item.call)
	}
	corpus := proc.fuzzer.corpusSnapshot()
	for i := 0; i < 100; i++ {
		p := item.p.Clone()
		p.Mutate(proc.rnd, programLength, proc.fuzzer.choiceTable, corpus)
		Logf(1, "#%v: smash mutated", proc.pid)
		proc.execute(proc.fuzzer.execOpts, p, false, false, false, false, StatSmash)
	}
	if compsSupported {
		proc.executeHintSeed(item.p, item.call)
	}
}

func (proc *Proc) failCall(p *prog.Prog, call int) {
	for nth := 0; nth < 100; nth++ {
		Logf(1, "#%v: injecting fault into call %v/%v", proc.pid, call, nth)
		opts := *proc.fuzzer.execOpts
		opts.Flags |= ipc.FlagInjectFault
		opts.FaultCall = call
		opts.FaultNth = nth
		info := proc.executeRaw(&opts, p, StatSmash)
		if info != nil && len(info) > call && !info[call].FaultInjected {
			break
		}
	}
}

func (proc *Proc) executeHintSeed(p *prog.Prog, call int) {
	Logf(1, "#%v: collecting comparisons", proc.pid)
	// First execute the original program to dump comparisons from KCOV.
	info := proc.execute(proc.fuzzer.execOpts, p, true, false, false, true, StatSeed)
	if info == nil {
		return
	}

	// Then mutate the initial program for every match between
	// a syscall argument and a comparison operand.
	// Execute each of such mutants to check if it gives new coverage.
	p.MutateWithHints(call, info[call].Comps, func(p *prog.Prog) {
		Logf(1, "#%v: executing comparison hint", proc.pid)
		proc.execute(proc.fuzzer.execOpts, p, false, false, false, false, StatHint)
	})
}

func (proc *Proc) execute(execOpts *ipc.ExecOpts, p *prog.Prog,
	needComps, minimized, candidate, noCollide bool, stat Stat) []ipc.CallInfo {

	opts := *execOpts
	if needComps {
		if !compsSupported {
			panic("compsSupported==false and execute() called with needComps")
		}
		opts.Flags |= ipc.FlagCollectComps
	}
	if noCollide {
		opts.Flags &= ^ipc.FlagCollide
	}
	info := proc.executeRaw(&opts, p, stat)
	signalMu.RLock()
	defer signalMu.RUnlock()

	for i, inf := range info {
		if !cover.SignalNew(maxSignal, inf.Signal) {
			continue
		}
		diff := cover.SignalDiff(maxSignal, inf.Signal)

		signalMu.RUnlock()
		signalMu.Lock()
		cover.SignalAdd(maxSignal, diff)
		cover.SignalAdd(newSignal, diff)
		signalMu.Unlock()
		signalMu.RLock()

		proc.fuzzer.workQueue.enqueue(&WorkTriage{
			p:         p.Clone(),
			call:      i,
			signal:    append([]uint32{}, inf.Signal...),
			candidate: candidate,
			minimized: minimized,
		})
	}
	return info
}

var logMu sync.Mutex

func (proc *Proc) executeRaw(opts *ipc.ExecOpts, p *prog.Prog, stat Stat) []ipc.CallInfo {
	pid := proc.pid
	if opts.Flags&ipc.FlagDedupCover == 0 {
		panic("dedup cover is not enabled")
	}

	// Limit concurrency window and do leak checking once in a while.
	ticket := proc.fuzzer.gate.Enter()
	defer proc.fuzzer.gate.Leave(ticket)

	strOpts := ""
	if opts.Flags&ipc.FlagInjectFault != 0 {
		strOpts = fmt.Sprintf(" (fault-call:%v fault-nth:%v)", opts.FaultCall, opts.FaultNth)
	}

	// The following output helps to understand what program crashed kernel.
	// It must not be intermixed.
	switch *flagOutput {
	case "none":
		// This case intentionally left blank.
	case "stdout":
		data := p.Serialize()
		logMu.Lock()
		Logf(0, "executing program %v%v:\n%s", pid, strOpts, data)
		logMu.Unlock()
	case "dmesg":
		fd, err := syscall.Open("/dev/kmsg", syscall.O_WRONLY, 0)
		if err == nil {
			buf := new(bytes.Buffer)
			fmt.Fprintf(buf, "syzkaller: executing program %v%v:\n%s", pid, strOpts, p.Serialize())
			syscall.Write(fd, buf.Bytes())
			syscall.Close(fd)
		}
	case "file":
		f, err := os.Create(fmt.Sprintf("%v-%v.prog", *flagName, pid))
		if err == nil {
			if strOpts != "" {
				fmt.Fprintf(f, "#%v\n", strOpts)
			}
			f.Write(p.Serialize())
			f.Close()
		}
	}

	try := 0
retry:
	atomic.AddUint64(&proc.fuzzer.stats[stat], 1)
	output, info, failed, hanged, err := proc.env.Exec(opts, p)
	if failed {
		// BUG in output should be recognized by manager.
		Logf(0, "BUG: executor-detected bug:\n%s", output)
		// Don't return any cover so that the input is not added to corpus.
		return nil
	}
	if err != nil {
		if _, ok := err.(ipc.ExecutorFailure); ok || try > 10 {
			panic(err)
		}
		try++
		Logf(4, "fuzzer detected executor failure='%v', retrying #%d\n", err, (try + 1))
		debug.FreeOSMemory()
		time.Sleep(time.Second)
		goto retry
	}
	Logf(2, "result failed=%v hanged=%v: %v\n", failed, hanged, string(output))
	return info
}
