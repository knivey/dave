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
	logger       logxi.Logger
	loggerDB     logxi.Logger
	loggerStore  logxi.Logger
	loggerUpload logxi.Logger
	loggerThumbs logxi.Logger
	loggerEvents logxi.Logger
	loggerImport logxi.Logger

	logFile *os.File
)

// initLogger sets up the file+stderr loggers. Failures here are fatal:
// every package-level logger would stay nil and the first log call
// would panic far from the root cause. Better to die at startup with
// the actual error in hand.
func initLogger(exeDir string) {
	logsDir := filepath.Join(exeDir, "logs")
	if err := os.MkdirAll(logsDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "error creating logs directory: %v\n", err)
		os.Exit(1)
	}

	name := filepath.Join(logsDir, fmt.Sprintf("imgsite-%s.log", time.Now().Format("2006-01-02")))
	f, err := os.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error opening log file: %v\n", err)
		os.Exit(1)
	}
	logFile = f

	writer := logxi.NewConcurrentWriter(io.MultiWriter(f, os.Stderr))

	if os.Getenv("LOGXI_FORMAT") == "" {
		logxi.ProcessLogxiFormatEnv("maxcol=9999")
	}

	logger = logxi.NewLogger(writer, "imgsite")
	logger.SetLevel(logxi.LevelAll)

	loggerDB = logxi.NewLogger(writer, "db")
	loggerDB.SetLevel(logxi.LevelAll)

	loggerStore = logxi.NewLogger(writer, "store")
	loggerStore.SetLevel(logxi.LevelAll)

	loggerUpload = logxi.NewLogger(writer, "upload")
	loggerUpload.SetLevel(logxi.LevelAll)

	loggerThumbs = logxi.NewLogger(writer, "thumbs")
	loggerThumbs.SetLevel(logxi.LevelAll)

	loggerEvents = logxi.NewLogger(writer, "events")
	loggerEvents.SetLevel(logxi.LevelAll)

	loggerImport = logxi.NewLogger(writer, "import")
	loggerImport.SetLevel(logxi.LevelAll)
}

func closeLogger() {
	if logFile != nil {
		logFile.Close()
		logFile = nil
	}
}
