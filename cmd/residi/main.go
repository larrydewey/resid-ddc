// residi runs a Resid program by interpretation, independently of the
// Resid compiler: it is the second implementation for diverse double
// compiling (see README.md).
package main

import (
	"fmt"
	"os"
	"runtime/pprof"
	"strconv"
	"strings"
	"time"

	"github.com/larrydewey/resid-ddc/interp"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: residi [--parse-only] [--lib DIR] [--stop-at-exec] program.resid [args...]")
		os.Exit(2)
	}
	var libs []string
	parseOnly, stopExec, check := false, false, false
	i := 1
	for ; i < len(os.Args) && strings.HasPrefix(os.Args[i], "--"); i++ {
		switch os.Args[i] {
		case "--parse-only":
			parseOnly = true
		case "--check":
			check = true
		case "--stop-at-exec":
			stopExec = true
		case "--lib":
			i++
			libs = append(libs, os.Args[i])
		}
	}
	prog, err := interp.Load(os.Args[i], libs)
	if err != nil {
		fmt.Fprintln(os.Stderr, "residi:", err)
		os.Exit(1)
	}
	if check {
		errs := interp.CheckAll(prog)
		for _, e := range errs {
			fmt.Println(e)
		}
		fmt.Printf("%d functions failed to compile\n", len(errs))
		return
	}
	if parseOnly {
		n := 0
		for _, m := range prog.Mods {
			n += len(m.Fns)
		}
		fmt.Printf("parsed %d modules, %d functions\n", len(prog.Mods), n)
		return
	}
	if pf := os.Getenv("RESIDI_CPUPROFILE"); pf != "" {
		f, _ := os.Create(pf)
		pprof.StartCPUProfile(f)
		if secs, _ := strconv.Atoi(os.Getenv("RESIDI_PROFSECS")); secs > 0 {
			go func() {
				time.Sleep(time.Duration(secs) * time.Second)
				pprof.StopCPUProfile()
				f.Close()
				os.Exit(124)
			}()
		}
		code := interp.Run(prog, os.Args[i:], interp.Options{StopAtExec: stopExec})
		pprof.StopCPUProfile()
		os.Exit(code)
	}
	os.Exit(interp.Run(prog, os.Args[i:], interp.Options{StopAtExec: stopExec}))
}
