package main

import (
	"fmt"
	"io"
	"sync"
	"time"
)

type birdseyeState int

const (
	stateUnstarted birdseyeState = iota
	stateInit
	stateSleep
	stateRun
	stateError
	stateDone
	stateMax
)

var spinner = []rune(".oOo.")

func (s birdseyeState) Char() string {
	switch s {
	case stateUnstarted:
		return " "
	case stateInit:
		return "I"
	case stateSleep:
		return "S"
	case stateRun:
		return string(spinner[time.Now().Second()%len(spinner)])
	case stateError:
		return "E"
	case stateDone:
		return "D"
	default:
		return fmt.Sprintf("BUG: state %d unknown", s)
	}
}

func (s birdseyeState) String() string {
	switch s {
	case stateUnstarted:
		return "unstarted"
	case stateInit:
		return "init"
	case stateSleep:
		return "sleep"
	case stateRun:
		return "run"
	case stateError:
		return "error"
	case stateDone:
		return "done"
	default:
		return fmt.Sprintf("BUG: state %d unknown", s)
	}
}

type birdseye struct {
	mu      sync.Mutex
	printed bool
	out     io.Writer
	states  []birdseyeState
}

func (b *birdseye) status(num int, state birdseyeState) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.states[num] = state
}

func (b *birdseye) print() {
	b.mu.Lock()
	defer b.mu.Unlock()
	var line string
	for _, s := range b.states {
		line += s.Char()
	}
	if b.printed {
		fmt.Fprintf(b.out, "\r")
	} else {
		b.printed = true
	}
	fmt.Fprintf(b.out, "\r[%s] ", line)
}

func (b *birdseye) flush() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.printed {
		fmt.Fprintf(b.out, "\n")
	}
}
