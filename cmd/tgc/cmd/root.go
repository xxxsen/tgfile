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

func initContext(ctx *Context, cfgs []string) error {
	var c *config.Config
	var err error
	for _, cfg := range cfgs {
		c, err = config.Parse(cfg)
		if err != nil {
			continue
		}
	}
	if err != nil {
		return fmt.Errorf("no valid config file found, last err:%w", err)
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
		envConfigFile, _ := os.LookupEnv(defaultConfigFileEnv)
		return initContext(ctx, []string{configFile, "/etc/tgc/tgc_config.json", "C:/tgc/tgc_config.json", envConfigFile})
	}
	rootCmd.PersistentFlags().StringVarP(&configFile, "config", "c", "", "config file")
	return rootCmd
}
