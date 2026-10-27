package main

import (
	"errors"
	"unsafe"

	"golang.org/x/sys/windows"
)

func newSampleClock() (sampleClock, error) {
	kernel := windows.NewLazySystemDLL("kernel32.dll")
	frequencyProc := kernel.NewProc("QueryPerformanceFrequency")
	counterProc := kernel.NewProc("QueryPerformanceCounter")
	var frequency int64
	if ok, _, _ := frequencyProc.Call(uintptr(unsafe.Pointer(&frequency))); ok == 0 || frequency <= 0 {
		return sampleClock{}, errors.New("Windows performance-counter frequency is unavailable")
	}
	read := func() (int64, error) {
		var counter int64
		if ok, _, _ := counterProc.Call(uintptr(unsafe.Pointer(&counter))); ok == 0 {
			return 0, errors.New("Windows performance counter is unavailable")
		}
		return counter, nil
	}
	return sampleClock{name: "QueryPerformanceCounter", frequency: float64(frequency), read: read}, nil
}
