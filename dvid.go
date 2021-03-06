// NOTE: No messages should be written to stdout because DVID can be used as a
// client where results of returned data are written out to stdout, e.g., a GET of data.
// Status and other informational messages should be reserved for the server package,
// executed during a 'serve' command.

package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/signal"
	"runtime"
	"runtime/pprof"
	"strings"
	"syscall"
	"time"

	"github.com/janelia-flyem/dvid/datastore"
	"github.com/janelia-flyem/dvid/dvid"
	"github.com/janelia-flyem/dvid/server"
	"github.com/janelia-flyem/dvid/storage"

	// Declare the data types this DVID executable will support
	_ "github.com/janelia-flyem/dvid/datatype/keyvalue"
	_ "github.com/janelia-flyem/dvid/datatype/labelmap"
	_ "github.com/janelia-flyem/dvid/datatype/labels64"
	_ "github.com/janelia-flyem/dvid/datatype/multichan16"
	_ "github.com/janelia-flyem/dvid/datatype/multiscale2d"
	_ "github.com/janelia-flyem/dvid/datatype/voxels"
)

var (
	// Path to datastore.
	datastorePath string

	// Display usage if true.
	showHelp = flag.Bool("help", false, "")

	// Use CRC32 for corruption detection.  Note that some storage engines
	// will already have error detection and this is at the DVID level, not
	// the storage engine (i.e., database) level.
	useCRC32 = flag.Bool("crc32", false, "")

	// List the supported data types if true.
	showTypes = flag.Bool("types", false, "")

	// Run in debug mode if true.
	runDebug = flag.Bool("debug", false, "")

	// Run in benchmark mode if true.
	runBenchmark = flag.Bool("benchmark", false, "")

	// Profile CPU usage using standard gotest system.
	cpuprofile = flag.String("cpuprofile", "", "")

	// Profile memory usage using standard gotest system.
	memprofile = flag.String("memprofile", "", "")

	// Path to web client directory.  Leave unset for default pages.
	clientDir = flag.String("webclient", "", "")

	// Address for rpc communication.
	rpcAddress = flag.String("rpc", server.DefaultRPCAddress, "")

	// Address for http communication
	httpAddress = flag.String("http", server.DefaultWebAddress, "")

	// Number of logical CPUs to use for DVID.
	useCPU = flag.Int("numcpu", 0, "")

	// Number of seconds to wait trying to get exclusive access to DVID datastore.
	timeout = flag.Int("timeout", 0, "")

	// Accept and send stdin to server for use in commands if true.
	useStdin = flag.Bool("stdin", false, "")
)

const helpMessage = `
dvid is a distributed, versioned image-oriented datastore

Usage: dvid [options] <command>

      -webclient  =string   Path to web client directory.  Leave unset for default pages.
      -rpc        =string   Address for RPC communication.
      -http       =string   Address for HTTP communication.
      -cpuprofile =string   Write CPU profile to this file.
      -memprofile =string   Write memory profile to this file on ctrl-C.
      -numcpu     =number   Number of logical CPUs to use for DVID.
      -timeout    =number   Seconds to wait trying to get exclusive access to datastore.
      -stdin      (flag)    Accept and send stdin to server for use in commands.
      -crc32      (flag)    Use CRC32 checksum to detect corruption.
      -types      (flag)    Show compiled DVID data types
      -debug      (flag)    Run in debug mode.  Verbose.
      -benchmark  (flag)    Run in benchmarking mode. 
  -h, -help       (flag)    Show help message

  For profiling, please refer to this excellent article:
  http://blog.golang.org/2011/06/profiling-go-programs.html

Commands that can be performed without a running server:

	about
	help
	init   <datastore path>
	serve  <datastore path>
	repair <datastore path>

`

const helpServerMessage = `
For further information, launch the DVID server (enter "dvid serve"), then use
a web browser to visit the DVID web server (%q by default).
`

var usage = func() {
	// Print local DVID help
	fmt.Printf(helpMessage)

	// Print server DVID help if available
	err := DoCommand(dvid.Command([]string{"help"}))
	if err != nil {
		fmt.Printf(helpServerMessage, server.DefaultWebAddress)
	}
}

func currentDir() string {
	currentDir, err := os.Getwd()
	if err != nil {
		log.Fatalln("Could not get current directory:", err)
	}
	return currentDir
}

func main() {
	flag.BoolVar(showHelp, "h", false, "Show help message")
	flag.Usage = usage
	flag.Parse()

	if flag.NArg() >= 1 && strings.ToLower(flag.Args()[0]) == "help" {
		*showHelp = true
	}

	if *runDebug {
		dvid.Mode = dvid.Debug
	}
	if *runBenchmark {
		dvid.Mode = dvid.Benchmark
	}
	if *timeout != 0 {
		server.TimeoutSecs = *timeout
	}
	if *useCRC32 {
		dvid.DefaultChecksum = dvid.CRC32
	}

	if *showHelp || flag.NArg() == 0 {
		flag.Usage()
		os.Exit(0)
	}
	if *showTypes {
		fmt.Println(datastore.CompiledTypeChart())
		os.Exit(0)
	}

	if *cpuprofile != "" {
		f, err := os.Create(*cpuprofile)
		if err != nil {
			log.Fatal(err)
		}
		pprof.StartCPUProfile(f)
		defer pprof.StopCPUProfile()
	}

	// Determine numer of logical CPUs on local machine and unless overridden, use
	// all of them.
	numCPU := runtime.NumCPU()
	if *useCPU != 0 {
		dvid.NumCPU = *useCPU
	} else if flag.NArg() >= 1 && flag.Args()[0] == "serve" {
		dvid.NumCPU = numCPU
	} else {
		dvid.NumCPU = 1
	}
	runtime.GOMAXPROCS(dvid.NumCPU)

	// Capture ctrl+c and other interrupts.  Then handle graceful shutdown.
	stopSig := make(chan os.Signal)
	go func() {
		for sig := range stopSig {
			log.Printf("Stop signal captured: %q.  Shutting down...\n", sig)
			if *memprofile != "" {
				log.Printf("Storing memory profiling to %s...\n", *memprofile)
				f, err := os.Create(*memprofile)
				if err != nil {
					log.Fatal(err)
				}
				pprof.WriteHeapProfile(f)
				f.Close()
			}
			if *cpuprofile != "" {
				log.Printf("Stopping CPU profiling to %s...\n", *cpuprofile)
				pprof.StopCPUProfile()
			}
			server.Shutdown()
			time.Sleep(1 * time.Second)
			os.Exit(0)
		}
	}()
	signal.Notify(stopSig, os.Interrupt, os.Kill, syscall.SIGTERM)

	command := dvid.Command(flag.Args())
	if err := DoCommand(command); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
}

// DoCommand serves as a switchboard for commands, handling local ones and
// sending via rpc those commands that need a running server.
func DoCommand(cmd dvid.Command) error {
	if len(cmd) == 0 {
		return fmt.Errorf("Blank command!")
	}

	switch cmd.Name() {
	// Handle commands that don't require server connection
	case "init":
		return DoInit(cmd)
	case "serve":
		return DoServe(cmd)
	case "repair":
		return DoRepair(cmd)
	case "about":
		fmt.Println(datastore.Versions())
	// Send everything else to server via DVID terminal
	default:
		client := server.NewClient(*rpcAddress)
		request := datastore.Request{Command: cmd}
		if *useStdin {
			var err error
			request.Input, err = ioutil.ReadAll(os.Stdin)
			if err != nil {
				return fmt.Errorf("Error in reading from standard input: %s", err.Error())
			}
		}
		return client.Send(request)
	}
	return nil
}

// DoInit performs the "init" command, creating a new DVID datastore.
func DoInit(cmd dvid.Command) error {
	datastorePath := cmd.Argument(1)
	if datastorePath == "" {
		return fmt.Errorf("init command must be followed by the path to the datastore")
	}
	create := true
	return datastore.Init(datastorePath, create, cmd.Settings())
}

// DoRepair performs the "repair" command, trying to repair a storage engine
func DoRepair(cmd dvid.Command) error {
	datastorePath := cmd.Argument(1)
	if datastorePath == "" {
		return fmt.Errorf("repair command must be followed by the path to the datastore")
	}
	if err := storage.RepairStore(datastorePath, cmd.Settings()); err != nil {
		return err
	}
	fmt.Printf("Ran repair on database at %s.\n", datastorePath)
	return nil
}

// DoServe opens a datastore then creates both web and rpc servers for the datastore
func DoServe(cmd dvid.Command) error {
	datastorePath := cmd.Argument(1)
	if datastorePath == "" {
		return fmt.Errorf("serve command must be followed by the path to the datastore")
	}
	if service, err := server.OpenDatastore(datastorePath); err != nil {
		return err
	} else {
		if err := service.Serve(*httpAddress, *clientDir, *rpcAddress); err != nil {
			return err
		}
	}
	return nil
}
