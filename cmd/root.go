// Package cmd the package implementing all of cli interface of k6
package cmd

import (
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	stdlog "log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"go.k6.io/k6/cmd/state"
	"go.k6.io/k6/errext"
	"go.k6.io/k6/lib/consts"
	"go.k6.io/k6/log"
)

const waitRemoteLoggerTimeout = time.Second * 5

func parseEnvKeyValue(kv string) (string, string) {
	if idx := strings.IndexRune(kv, '='); idx != -1 {
		return kv[:idx], kv[idx+1:]
	}
	return kv, ""
}

func buildEnvMap(environ []string) map[string]string {
	env := make(map[string]string, len(environ))
	for _, kv := range environ {
		k, v := parseEnvKeyValue(kv)
		env[k] = v
	}
	return env
}

// This is to keep all fields needed for the main/root k6 command
type rootCommand struct {
	globalState *globalState

	cmd            *cobra.Command
	loggerStopped  <-chan struct{}
	loggerIsRemote bool
}

func newRootCommand(gs *globalState) *rootCommand {
	c := &rootCommand{
		globalState: gs,
	}
	// the base command when called without any subcommands.
	rootCmd := &cobra.Command{
		Use:               "k6",
		Short:             "a next-generation load generator",
		Long:              "\n" + getBanner(c.globalState.flags.noColor || !c.globalState.stdOut.isTTY),
		SilenceUsage:      true,
		SilenceErrors:     true,
		PersistentPreRunE: c.persistentPreRunE,
	}

	rootCmd.PersistentFlags().AddFlagSet(rootCmdPersistentFlagSet(gs))
	rootCmd.SetArgs(gs.args[1:])
	rootCmd.SetOut(gs.stdOut)
	rootCmd.SetErr(gs.stdErr) // TODO: use gs.logger.WriterLevel(logrus.ErrorLevel)?
	rootCmd.SetIn(gs.stdIn)

	subCommands := []func(*globalState) *cobra.Command{
		getCmdArchive, getCmdCloud, getCmdConvert, getCmdInspect,
		getCmdLogin, getCmdPause, getCmdResume, getCmdScale, getCmdRun,
		getCmdStats, getCmdStatus, getCmdVersion,
	}

	for _, sc := range subCommands {
		rootCmd.AddCommand(sc(gs))
	}

	c.cmd = rootCmd
	return c
}

func (c *rootCommand) persistentPreRunE(cmd *cobra.Command, args []string) error {
	var err error

	c.loggerStopped, err = c.setupLoggers()
	if err != nil {
		return err
	}
	select {
	case <-c.loggerStopped:
	default:
		c.loggerIsRemote = true
	}

	stdlog.SetOutput(c.globalState.logger.Writer())
	c.globalState.logger.Debugf("k6 version: v%s", consts.FullVersion())
	return nil
}

func (c *rootCommand) execute() {
	ctx, cancel := context.WithCancel(c.globalState.ctx)
	defer cancel()
	c.globalState.ctx = ctx

	err := c.cmd.Execute()
	if err == nil {
		cancel()
		c.waitRemoteLogger()
		return
	}

	exitCode := -1
	var ecerr errext.HasExitCode
	if errors.As(err, &ecerr) {
		exitCode = int(ecerr.ExitCode())
	}

	errText := err.Error()
	var xerr errext.Exception
	if errors.As(err, &xerr) {
		errText = xerr.StackTrace()
	}

	fields := logrus.Fields{}
	var herr errext.HasHint
	if errors.As(err, &herr) {
		fields["hint"] = herr.Hint()
	}

	c.globalState.logger.WithFields(fields).Error(errText)
	if c.loggerIsRemote {
		c.globalState.fallbackLogger.WithFields(fields).Error(errText)
		cancel()
		c.waitRemoteLogger()
	}

	c.globalState.osExit(exitCode)
}

// Execute adds all child commands to the root command sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() {
	outMx := &sync.Mutex{}
	gs := state.NewGlobalState(
		context.Background(),
		newConsoleWriter(os.Stdout, outMx),
		newConsoleWriter(os.Stderr, outMx),
	)

	newRootCommand(gs).execute()
}

func (c *rootCommand) waitRemoteLogger() {
	if c.loggerIsRemote {
		select {
		case <-c.loggerStopped:
		case <-time.After(waitRemoteLoggerTimeout):
			c.globalState.fallbackLogger.Errorf("Remote logger didn't stop in %s", waitRemoteLoggerTimeout)
		}
	}
}

func rootCmdPersistentFlagSet(gs *globalState) *pflag.FlagSet {
	flags := pflag.NewFlagSet("", pflag.ContinueOnError)
	// TODO: refactor this config, the default value management with pflag is
	// simply terrible... :/
	//
	// We need to use `gs.flags.<value>` both as the destination and as
	// the value here, since the config values could have already been set by
	// their respective environment variables. However, we then also have to
	// explicitly set the DefValue to the respective default value from
	// `gs.defaultFlags.<value>`, so that the `k6 --help` message is
	// not messed up...

	flags.StringVar(&gs.flags.logOutput, "log-output", gs.flags.logOutput,
		"change the output for k6 logs, possible values are stderr,stdout,none,loki[=host:port],file[=./path.fileformat]")
	flags.Lookup("log-output").DefValue = gs.defaultFlags.logOutput

	flags.StringVar(&gs.flags.logFormat, "logformat", gs.flags.logFormat, "log output format")
	oldLogFormat := flags.Lookup("logformat")
	oldLogFormat.Hidden = true
	oldLogFormat.Deprecated = "log-format"
	oldLogFormat.DefValue = gs.defaultFlags.logFormat
	flags.StringVar(&gs.flags.logFormat, "log-format", gs.flags.logFormat, "log output format")
	flags.Lookup("log-format").DefValue = gs.defaultFlags.logFormat

	flags.StringVarP(&gs.flags.configFilePath, "config", "c", gs.flags.configFilePath, "JSON config file")
	// And we also need to explicitly set the default value for the usage message here, so things
	// like `K6_CONFIG="blah" k6 run -h` don't produce a weird usage message
	flags.Lookup("config").DefValue = gs.defaultFlags.configFilePath
	must(cobra.MarkFlagFilename(flags, "config"))

	flags.BoolVar(&gs.flags.noColor, "no-color", gs.flags.noColor, "disable colored output")
	flags.Lookup("no-color").DefValue = strconv.FormatBool(gs.defaultFlags.noColor)

	// TODO: support configuring these through environment variables as well?
	// either with croconf or through the hack above...
	flags.BoolVarP(&gs.flags.verbose, "verbose", "v", gs.defaultFlags.verbose, "enable verbose logging")
	flags.BoolVarP(&gs.flags.quiet, "quiet", "q", gs.defaultFlags.quiet, "disable progress updates")
	flags.StringVarP(&gs.flags.address, "address", "a", gs.defaultFlags.address, "address for the REST API server")

	return flags
}

// RawFormatter it does nothing with the message just prints it
type RawFormatter struct{}

// Format renders a single log entry
func (f RawFormatter) Format(entry *logrus.Entry) ([]byte, error) {
	return append([]byte(entry.Message), '\n'), nil
}

// The returned channel will be closed when the logger has finished flushing and pushing logs after
// the provided context is closed. It is closed if the logger isn't buffering and sending messages
// Asynchronously
func (c *rootCommand) setupLoggers() (<-chan struct{}, error) {
	ch := make(chan struct{})
	close(ch)

	if c.globalState.flags.verbose {
		c.globalState.logger.SetLevel(logrus.DebugLevel)
	}

	loggerForceColors := false // disable color by default
	switch line := c.globalState.flags.logOutput; {
	case line == "stderr":
		loggerForceColors = !c.globalState.flags.noColor && c.globalState.stdErr.isTTY
		c.globalState.logger.SetOutput(c.globalState.stdErr)
	case line == "stdout":
		loggerForceColors = !c.globalState.flags.noColor && c.globalState.stdOut.isTTY
		c.globalState.logger.SetOutput(c.globalState.stdOut)
	case line == "none":
		c.globalState.logger.SetOutput(ioutil.Discard)

	case strings.HasPrefix(line, "loki"):
		ch = make(chan struct{}) // TODO: refactor, get it from the constructor
		hook, err := log.LokiFromConfigLine(c.globalState.ctx, c.globalState.fallbackLogger, line, ch)
		if err != nil {
			return nil, err
		}
		c.globalState.logger.AddHook(hook)
		c.globalState.logger.SetOutput(ioutil.Discard) // don't output to anywhere else
		c.globalState.flags.logFormat = "raw"

	case strings.HasPrefix(line, "file"):
		ch = make(chan struct{}) // TODO: refactor, get it from the constructor
		hook, err := log.FileHookFromConfigLine(
			c.globalState.ctx, c.globalState.fs, c.globalState.getwd,
			c.globalState.fallbackLogger, line, ch,
		)
		if err != nil {
			return nil, err
		}

		c.globalState.logger.AddHook(hook)
		c.globalState.logger.SetOutput(ioutil.Discard)

	default:
		return nil, fmt.Errorf("unsupported log output '%s'", line)
	}

	switch c.globalState.flags.logFormat {
	case "raw":
		c.globalState.logger.SetFormatter(&RawFormatter{})
		c.globalState.logger.Debug("Logger format: RAW")
	case "json":
		c.globalState.logger.SetFormatter(&logrus.JSONFormatter{})
		c.globalState.logger.Debug("Logger format: JSON")
	default:
		c.globalState.logger.SetFormatter(&logrus.TextFormatter{
			ForceColors: loggerForceColors, DisableColors: c.globalState.flags.noColor,
		})
		c.globalState.logger.Debug("Logger format: TEXT")
	}
	return ch, nil
}
