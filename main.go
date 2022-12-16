package main

import (
	"log"
	"os"
	"os/signal"

	"github.com/jaronoff97/opamp-operator-server/data"
	"github.com/jaronoff97/opamp-operator-server/opampsrv"
	"github.com/jaronoff97/opamp-operator-server/uisrv"
)

var logger = log.New(log.Default().Writer(), "[MAIN] ", log.Default().Flags()|log.Lmsgprefix|log.Lmicroseconds)

func main() {
	curDir, err := os.Getwd()
	if err != nil {
		panic(err)
	}

	logger.Println("OpAMP Server starting...")

	uisrv.Start(curDir)
	opampSrv := opampsrv.NewServer(&data.AllAgents)
	opampSrv.Start()

	logger.Println("OpAMP Server running...")

	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)
	<-interrupt

	logger.Println("OpAMP Server shutting down...")
	uisrv.Shutdown()
	opampSrv.Stop()
}
