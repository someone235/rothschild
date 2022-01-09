package main

import (
	"github.com/kaspanet/kaspad/domain/dagconfig"
	"github.com/kaspanet/kaspad/infrastructure/logger"
	"github.com/kaspanet/kaspad/util"
	"os"
	"path/filepath"

	"github.com/jessevdk/go-flags"
)

const (
	defaultLogFilename    = "rothschild.log"
	defaultErrLogFilename = "rothschild_err.log"
)

var (
	defaultHomeDir = util.AppDir("rothschild", false)
	// Default configuration options
	defaultLogFile    = filepath.Join(defaultHomeDir, defaultLogFilename)
	defaultErrLogFile = filepath.Join(defaultHomeDir, defaultErrLogFilename)
)

type configFlags struct {
	Profile             string `long:"profile" description:"Enable HTTP profiling on given port -- NOTE port must be between 1024 and 65536"`
	RPCServer           string `long:"rpcserver" short:"s" description:"RPC server to connect to"`
	AddressesFilePath   string `long:"addresses-file" short:"a" description:"path of file containing our and everybody else's addresses'"`
	TransactionInterval uint   `long:"transaction-interval" short:"i" description:"Time between transactions (in milliseconds; default:1000)"`
	ActiveNetParams     *dagconfig.Params
}

var cfg *configFlags

func activeConfig() *configFlags {
	return cfg
}

func parseConfig() error {
	cfg = &configFlags{}
	parser := flags.NewParser(cfg, flags.PrintErrors|flags.HelpFlag)

	_, err := parser.Parse()

	if err != nil {
		if err, ok := err.(*flags.Error); ok && err.Type == flags.ErrHelp {
			os.Exit(0)
		}
		return err
	}

	cfg.ActiveNetParams = &dagconfig.TestnetParams
	if cfg.TransactionInterval == 0 {
		cfg.TransactionInterval = 1000
	}

	log.SetLevel(logger.LevelInfo)
	initLogs(backendLog, defaultLogFile, defaultErrLogFile)

	return nil
}
