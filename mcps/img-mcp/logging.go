package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	logxi "github.com/mgutz/logxi/v1"
)

var (
	logger      logxi.Logger
	loggerQueue logxi.Logger
	loggerDB    logxi.Logger
	loggerTools logxi.Logger

	logFile *os.File
)

func initLogger(exeDir string) {
	logsDir := filepath.Join(exeDir, "logs")
	if err := os.MkdirAll(logsDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "error creating logs directory: %v\n", err)
		return
	}

	name := filepath.Join(logsDir, fmt.Sprintf("img-mcp-%s.log", time.Now().Format("2006-01-02")))
	f, err := os.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error opening log file: %v\n", err)
		return
	}
	logFile = f

	writer := logxi.NewConcurrentWriter(io.MultiWriter(f, os.Stderr))

	if os.Getenv("LOGXI_FORMAT") == "" {
		logxi.ProcessLogxiFormatEnv("maxcol=9999")
	}

	logger = logxi.NewLogger(writer, "img-mcp")
	logger.SetLevel(logxi.LevelAll)

	loggerQueue = logxi.NewLogger(writer, "queue")
	loggerQueue.SetLevel(logxi.LevelAll)

	loggerDB = logxi.NewLogger(writer, "db")
	loggerDB.SetLevel(logxi.LevelAll)

	loggerTools = logxi.NewLogger(writer, "tools")
	loggerTools.SetLevel(logxi.LevelAll)
}

func closeLogger() {
	if logFile != nil {
		logFile.Close()
		logFile = nil
	}
}
