package cmd

import (
	"fmt"
	"os"
	"tgfile/cmd/tgc/config"
	"tgfile/tgc"
	"tgfile/tgc/client"

	"github.com/spf13/cobra"
	"github.com/xxxsen/common/logger"
)

const (
	defaultConfigFileEnv = "TGC_CONFIG"
)

var cmds []CreateFunc

type Context struct {
	TGC    *tgc.TGFileClient
	Config *config.Config
}

type CreateFunc func(ctx *Context) *cobra.Command

func register(cr CreateFunc) {
	cmds = append(cmds, cr)
}

func initContext(ctx *Context, cfg string) error {
	c, err := config.Parse(cfg)
	if err != nil {
		return err
	}
	ctx.Config = c
	logger.Init("", c.LogLevel, 0, 0, 0, true)
	cli, err := client.New(client.WithSchema(c.Schema), client.WithHost(c.Host), client.WithAuth(c.AccessKey, c.SecretKey))
	if err != nil {
		return err
	}
	ctx.TGC = tgc.New(tgc.WithClient(cli), tgc.WithThread(c.Thread))
	return nil
}

func NewRoot() *cobra.Command {
	var configFile string
	ctx := &Context{}
	var rootCmd = &cobra.Command{
		Use:   "tgc",
		Short: "TGFile CLI tool",
	}
	for _, cr := range cmds {
		rootCmd.AddCommand(cr(ctx))
	}
	rootCmd.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		if len(configFile) == 0 {
			configFile, _ = os.LookupEnv(defaultConfigFileEnv)
		}
		if len(configFile) == 0 {
			return fmt.Errorf("no config file found")
		}
		return initContext(ctx, configFile)
	}
	rootCmd.PersistentFlags().StringVarP(&configFile, "config", "c", "", "config file")
	return rootCmd
}
