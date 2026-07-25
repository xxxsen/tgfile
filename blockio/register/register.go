package register

import (
	// Register built-in BlockIO implementations through their init functions.
	_ "github.com/xxxsen/tgfile/blockio/localfile"
	_ "github.com/xxxsen/tgfile/blockio/mem"
	_ "github.com/xxxsen/tgfile/blockio/telegram"
)
