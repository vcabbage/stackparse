# stackparse

Replaces hex values in stack traces with type information and human readable values.

Status: Works on my machine when I hold it right.

Known Issues:

* Code paths in stacktrace must match local filesystem paths.
* Pointer size and endianness of system running stackparse must be the same as system the trace is from.
* Does not work for functions implemented in assembly.
* Does not work for builtin functions.

Example:

``` go
package main

type S struct {
	A int
	b bool
	int
}

func main() {
	MyFunc(123, 36, nil, "hello", S{A: 76, b: true, int: 789}, [4]int{1, 2, 3, 5})
}

// If function is inlined there are no arguments to parse.

//go:noinline
func MyFunc(h int8, a int16, x chan int, s string, p S, e [4]int) {
	panic("dang")
}
```

(w/ `GOTRACEBACK=crash` to illustrate runtime information)
```
panic: dang

goroutine 1 [running]:
panic(0x45b1a0, 0x47ced0)
        /home/kale/goroots/go1.12/src/runtime/panic.go:565 +0x2c5 fp=0xc000048710 sp=0xc000048680 pc=0x423135
main.MyFunc(h int8(123), a int16(36), x (chan int)(nil), s string{data: 0x46f1c5, len: 5}, p S{A: int(76), b: bool(true), int: int(789)}, e [4]int[1, 2, 3, ...])
        /home/kale/go/src/github.com/vcabbage/example/main.go:17 +0x39 fp=0xc000048730 sp=0xc000048710 pc=0x44f9f9
main.main()
        /home/kale/go/src/github.com/vcabbage/example/main.go:10 +0x7a fp=0xc000048798 sp=0xc000048730 pc=0x44f9aa
runtime.main()
        /home/kale/goroots/go1.12/src/runtime/proc.go:200 +0x20c fp=0xc0000487e0 sp=0xc000048798 pc=0x424e1c
runtime.goexit()
        /home/kale/goroots/go1.12/src/runtime/asm_amd64.s:1337 +0x1 fp=0xc0000487e8 sp=0xc0000487e0 pc=0x449631

goroutine 2 [force gc (idle)]:
runtime.gopark(unlockf (func(*g, unsafe.Pointer) bool)(0x4738f8), lock unsafe.Pointer(0x4c1a80), reason waitReason(16), traceEv byte(20), traceskip int(1))
        /home/kale/goroots/go1.12/src/runtime/proc.go:301 +0xef fp=0xc000048fb0 sp=0xc000048f90 pc=0x4251ff
runtime.goparkunlock(lock ...)
        /home/kale/goroots/go1.12/src/runtime/proc.go:307
runtime.forcegchelper()
        /home/kale/goroots/go1.12/src/runtime/proc.go:250 +0xb7 fp=0xc000048fe0 sp=0xc000048fb0 pc=0x4250a7
runtime.goexit()
        /home/kale/goroots/go1.12/src/runtime/asm_amd64.s:1337 +0x1 fp=0xc000048fe8 sp=0xc000048fe0 pc=0x449631
created by runtime.init.5
        /home/kale/goroots/go1.12/src/runtime/proc.go:239 +0x35

goroutine 3 [GC sweep wait]:
runtime.gopark(unlockf (func(*g, unsafe.Pointer) bool)(0x4738f8), lock unsafe.Pointer(0x4c1b60), reason waitReason(12), traceEv byte(20), traceskip int(1))
        /home/kale/goroots/go1.12/src/runtime/proc.go:301 +0xef fp=0xc0000497a8 sp=0xc000049788 pc=0x4251ff
runtime.goparkunlock(lock ...)
        /home/kale/goroots/go1.12/src/runtime/proc.go:307
runtime.bgsweep(c (chan int)(0xc000066000))
        /home/kale/goroots/go1.12/src/runtime/mgcsweep.go:70 +0x9c fp=0xc0000497d8 sp=0xc0000497a8 pc=0x41a13c
runtime.goexit()
        /home/kale/goroots/go1.12/src/runtime/asm_amd64.s:1337 +0x1 fp=0xc0000497e0 sp=0xc0000497d8 pc=0x449631
created by runtime.gcenable
        /home/kale/goroots/go1.12/src/runtime/mgc.go:208 +0x58
signal: aborted (core dumped)
```

becomes

```
panic: dang

goroutine 1 [running]:
panic(0x45b1a0, 0x47ced0)
	/home/kale/goroots/go1.12/src/runtime/panic.go:565 +0x2c5 fp=0xc000048710 sp=0xc000048680 pc=0x423135
main.MyFunc(h int8(123), a int16(36), x (chan int)(nil), s string{data: 0x46f1c5, len: 5}, p S{A: int(76), b: bool(true), int: int(789)}, e [4]int[1, 2, 3, ...])
	/home/kale/go/src/github.com/vcabbage/example/main.go:17 +0x39 fp=0xc000048730 sp=0xc000048710 pc=0x44f9f9
main.main()
	/home/kale/go/src/github.com/vcabbage/example/main.go:10 +0x7a fp=0xc000048798 sp=0xc000048730 pc=0x44f9aa
runtime.main()
	/home/kale/goroots/go1.12/src/runtime/proc.go:200 +0x20c fp=0xc0000487e0 sp=0xc000048798 pc=0x424e1c
runtime.goexit()
	/home/kale/goroots/go1.12/src/runtime/asm_amd64.s:1337 +0x1 fp=0xc0000487e8 sp=0xc0000487e0 pc=0x449631

goroutine 2 [force gc (idle)]:
runtime.gopark(unlockf (func(*g, unsafe.Pointer) bool)(0x4738f8), lock unsafe.Pointer(0x4c1a80), reason waitReason(16), traceEv byte(20), traceskip int(1))
	/home/kale/goroots/go1.12/src/runtime/proc.go:301 +0xef fp=0xc000048fb0 sp=0xc000048f90 pc=0x4251ff
runtime.goparkunlock(lock ...)
	/home/kale/goroots/go1.12/src/runtime/proc.go:307
runtime.forcegchelper()
	/home/kale/goroots/go1.12/src/runtime/proc.go:250 +0xb7 fp=0xc000048fe0 sp=0xc000048fb0 pc=0x4250a7
runtime.goexit()
	/home/kale/goroots/go1.12/src/runtime/asm_amd64.s:1337 +0x1 fp=0xc000048fe8 sp=0xc000048fe0 pc=0x449631
created by runtime.init.5
	/home/kale/goroots/go1.12/src/runtime/proc.go:239 +0x35

goroutine 3 [GC sweep wait]:
runtime.gopark(unlockf (func(*g, unsafe.Pointer) bool)(0x4738f8), lock unsafe.Pointer(0x4c1b60), reason waitReason(12), traceEv byte(20), traceskip int(1))
	/home/kale/goroots/go1.12/src/runtime/proc.go:301 +0xef fp=0xc0000497a8 sp=0xc000049788 pc=0x4251ff
runtime.goparkunlock(lock ...)
	/home/kale/goroots/go1.12/src/runtime/proc.go:307
runtime.bgsweep(c (chan int)(0xc000066000))
	/home/kale/goroots/go1.12/src/runtime/mgcsweep.go:70 +0x9c fp=0xc0000497d8 sp=0xc0000497a8 pc=0x41a13c
runtime.goexit()
	/home/kale/goroots/go1.12/src/runtime/asm_amd64.s:1337 +0x1 fp=0xc0000497e0 sp=0xc0000497d8 pc=0x449631
created by runtime.gcenable
	/home/kale/goroots/go1.12/src/runtime/mgc.go:208 +0x58
signal: aborted (core dumped)


```