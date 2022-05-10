package main

import (
	"fmt"
	"log"
	"os"
	"strings"

	rootCmd "code-intelligence.com/cifuzz/internal/cmd/root"
	"code-intelligence.com/cifuzz/pkg/storage"
	"github.com/spf13/cobra/doc"
	"github.com/spf13/pflag"
)

func main() {
	flags := pflag.NewFlagSet("", pflag.ExitOnError)
	dir := flags.String("dir", ".", "target directory for the docs")

	if err := flags.Parse(os.Args); err != nil {
		log.Fatalf("unable to parse flags %v", err)
	}

	fs := storage.WrapFileSystem()
	cmd := rootCmd.New(fs)
	cmd.DisableAutoGenTag = true
	if err := doc.GenMarkdownTreeCustom(cmd, *dir, filePrepender, linkHandler); err != nil {
		log.Fatalf("error while generating markdown: %v", err)
	}
	fmt.Printf("successfully generated docs at %s\n", *dir)
}

func linkHandler(link string) string {
	return strings.TrimSuffix(link, ".md")
}

func filePrepender(filename string) string {
	return filename
}
