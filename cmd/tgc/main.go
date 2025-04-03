package main

import (
	"log"
	"tgfile/cmd/tgc/cmd"
)

func main() {
	if err := cmd.NewRoot().Execute(); err != nil {
		log.Printf("exec cmd failed, err:%v", err)
	}
}
