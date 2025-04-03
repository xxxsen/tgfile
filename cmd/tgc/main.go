package main

import (
	"log"

	"github.com/xxxsen/tgfile/cmd/tgc/cmd"
)

func main() {
	if err := cmd.NewRoot().Execute(); err != nil {
		log.Printf("exec cmd failed, err:%v", err)
	}
}
