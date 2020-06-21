package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"runtime/trace"
	"sync"
	"syscall"

	"github.com/kralamoure/d1postgres"
	"github.com/spf13/pflag"
	"go.uber.org/zap"

	"github.com/kralamoure/d1login"
)

const version = "v0.0.0"

var (
	printVersion bool
	debug        bool
	addr         string
	pgConnString string
)

var logger *zap.Logger

func main() {
	err := loadVars()
	if err != nil {
		if errors.Is(err, pflag.ErrHelp) {
			return
		}
		log.Println(err)
		os.Exit(2)
	}

	if printVersion {
		fmt.Println(version)
		return
	}

	if debug {
		logger, err = zap.NewDevelopment()
		if err != nil {
			log.Fatalln(err)
		}
	} else {
		logger, err = zap.NewProduction()
		if err != nil {
			log.Fatalln(err)
		}
	}

	err = run()
	if err != nil {
		logger.Fatal(err.Error())
	}
}

func run() error {
	defer logger.Sync()

	if debug {
		traceFile, err := os.Create("trace.out")
		if err != nil {
			return err
		}
		defer traceFile.Close()
		err = trace.Start(traceFile)
		if err != nil {
			return err
		}
		defer trace.Stop()
	}

	var wg sync.WaitGroup
	defer wg.Wait()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigCh)

	errCh := make(chan error)

	repo, err := d1postgres.NewDB(ctx, pgConnString)
	if err != nil {
		return err
	}

	svr, err := d1login.NewServer(d1login.Config{
		Addr:   addr,
		Repo:   repo,
		Logger: logger.Named("server"),
	})
	if err != nil {
		return err
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := svr.ListenAndServe(ctx)
		if err != nil {
			select {
			case errCh <- fmt.Errorf("error while listening and serving: %w", err):
			case <-ctx.Done():
			}
		}
	}()

	select {
	case sig := <-sigCh:
		logger.Info("received signal",
			zap.String("signal", sig.String()),
		)
	case err := <-errCh:
		logger.Error(err.Error())
		return err
	case <-ctx.Done():
	}
	return nil
}

func loadVars() error {
	flags := pflag.NewFlagSet("d1login", pflag.ContinueOnError)
	flags.BoolVarP(&printVersion, "version", "v", false, "Print version")
	flags.BoolVarP(&debug, "debug", "d", false, "Enable debug mode")
	flags.StringVarP(&addr, "address", "a", "0.0.0.0:5555", "server listener's address")
	flags.StringVarP(&pgConnString, "postgres", "p", "postgresql://user:password@host/database", "PostgreSQL connection string")
	flags.SortFlags = false
	return flags.Parse(os.Args)
}