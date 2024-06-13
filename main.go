package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	oplog "github.com/ethereum-optimism/optimism/op-service/log"
	opmetrics "github.com/ethereum-optimism/optimism/op-service/metrics"
	"github.com/ethereum-optimism/optimism/op-service/txmgr"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/log"
	"github.com/urfave/cli"
)

type ResourceItem struct {
	FilePath    string
	Possibility int

	fileData [][]byte
	seq      int64
}

type Resources struct {
	nameMapping []string
	data        map[string]*ResourceItem
}

func (r *Resources) RandomSelect() []byte {
	resourceIdx := rand.Intn(len(r.nameMapping))
	item := r.data[r.nameMapping[resourceIdx]]
	seq := atomic.AddInt64(&item.seq, 1)
	return item.fileData[int(seq%int64(len(item.fileData)))]
}

func parseDataSourceFile(fp string) ([][]byte, error) {
	data, err := os.ReadFile(fp)
	if err != nil {
		return nil, fmt.Errorf("read file=%v failed: %w", fp, err)
	}
	lines := bytes.Split(data, []byte("\n"))
	out := make([][]byte, 0, len(lines))
	for _, item := range lines {
		item = bytes.TrimSpace(item)
		n, err := hex.Decode(item, item)
		if err != nil {
			return nil, fmt.Errorf("decode data=%d fail", item)
		}
		if n > 0 {
			out = append(out, item[:n])
		}
	}
	return out, nil
}

func buildResources(fp string) (*Resources, error) {
	if len(fp) == 0 {
		return nil, nil
	}
	var resources Resources
	data, err := os.ReadFile(fp)
	if err != nil {
		return nil, fmt.Errorf("read file=%v fail: %w", fp, err)
	}
	if err := json.Unmarshal(data, &resources.data); err != nil {
		return nil, fmt.Errorf("parse file=%v fail: %w", fp, err)
	}
	mapping := make([]string, 0)
	for name, item := range resources.data {
		fileData, err := parseDataSourceFile(item.FilePath)
		if err != nil {
			return nil, fmt.Errorf("parse source file=%v failed: %w", name, err)
		}
		for i := 0; i < item.Possibility; i++ {
			mapping = append(mapping, name)
		}
		item.fileData = fileData
	}
	resources.nameMapping = mapping
	return &resources, nil
}

var logger log.Logger

type TxOverload struct {
	Distrbutor      *Distributor
	BytesPerSecond  int
	StartTime       time.Time
	NumDistributors int
	Resources       *Resources
}

func (t *TxOverload) generateTxCandidate() (txmgr.TxCandidate, error) {
	var to common.Address

	var data []byte
	if t.Resources == nil {
		data = make([]byte, t.BytesPerSecond)
		//dur := time.Since(t.StartTime)
		_, err := rand.Read(data)
		if err != nil {
			return txmgr.TxCandidate{}, err
		}
	} else {
		data = t.Resources.RandomSelect()
	}

	intrinsicGas, err := core.IntrinsicGas(data, nil, false, true, true, false)
	if err != nil {
		return txmgr.TxCandidate{}, err
	}
	return txmgr.TxCandidate{
		To:       &to,
		TxData:   data,
		GasLimit: intrinsicGas,
	}, nil
}

func (t *TxOverload) Start() {
	ctx := context.Background()
	t.Distrbutor.Start()

	const blockTimeMs = 2000
	tickRate := time.Duration(blockTimeMs/t.NumDistributors) * time.Millisecond
	ticker := time.NewTicker(tickRate)
	defer ticker.Stop()

	var backoff = tickRate
	var backingOff bool
	backoffFn := func(err error) {
		const maxBackoff = time.Second * 2
		switch {
		case err == nil && backingOff:
			backoff = tickRate
			backingOff = false
			ticker.Reset(tickRate)
		case err == ErrQueueFull:
			backingOff = true
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
			ticker.Reset(backoff)
			logger.Debug("backoff", "duration", backoff)
		}
	}

	for {
		select {
		case <-ticker.C:
			if t.StartTime.IsZero() {
				t.StartTime = time.Now()
			}
			candidate, err := t.generateTxCandidate()
			if err != nil {
				logger.Warn("unable to generate tx candidate", "err", err)
				continue
			}
			err = t.Distrbutor.Send(ctx, candidate)
			backoffFn(err)
		case <-ctx.Done():
			return
		}
	}
}

func Main(cliCtx *cli.Context) error {
	logCfg := oplog.ReadCLIConfig(cliCtx)
	if err := logCfg.Check(); err != nil {
		return err
	}
	logger = oplog.NewLogger(logCfg)
	txmgrCfg := txmgr.ReadCLIConfig(cliCtx)
	txmgrCfg.L1RPCURL = cliCtx.GlobalString(EthRpcFlag.Name) // should be named L2RPCURL but this will work just as well
	if err := txmgrCfg.Check(); err != nil {
		return err
	}

	resources, err := buildResources(cliCtx.GlobalString(FromFile.Name))
	if err != nil {
		return err
	}

	numDistributors := cliCtx.GlobalInt(NumDistributors.Name)
	distributors = keys[:numDistributors]

	metricsCfg := opmetrics.ReadCLIConfig(cliCtx)
	m := NewMetrics()
	if metricsCfg.Enabled {
		logger.Info("starting metrics server", "addr", metricsCfg.ListenAddr, "port", metricsCfg.ListenPort)
		go func() {
			if err := m.Serve(context.Background(), metricsCfg.ListenAddr, metricsCfg.ListenPort); err != nil {
				logger.Error("error starting metrics server", err)
			}
		}()
	}

	distributor, err := NewDistributor(txmgrCfg, logger, m)
	if err != nil {
		return err
	}

	t := &TxOverload{
		Distrbutor:      distributor,
		BytesPerSecond:  cliCtx.GlobalInt(DataRateFlag.Name),
		NumDistributors: numDistributors,
		Resources:       resources,
	}
	go t.Start()

	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, []os.Signal{
		os.Interrupt,
		os.Kill,
		syscall.SIGTERM,
		syscall.SIGQUIT,
	}...)
	<-interrupt

	return nil
}

func main() {
	oplog.SetupDefaults()
	app := cli.NewApp()
	app.Name = "tx-overload"
	app.Flags = flags
	app.Action = func(ctx *cli.Context) error {
		return Main(ctx)
	}
	err := app.Run(os.Args)
	if err != nil {
		log.Crit("Application failed", "message", err)
	}
}
