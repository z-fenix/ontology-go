package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/acme/ontology/internal/codegen"
)

func newSDKCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{Use: "sdk", Short: "Generate typed client SDKs"}
	cmd.AddCommand(newSDKGenerateCmd(stdout, stderr))
	return cmd
}

func newSDKGenerateCmd(stdout, stderr io.Writer) *cobra.Command {
	var lang, out, header string
	var version int64
	cmd := &cobra.Command{
		Use:   "generate [dir]",
		Short: "Generate a TypeScript client from the ontology (one self-contained file, no dependencies)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if lang != "ts" && lang != "typescript" {
				return fmt.Errorf("unsupported --lang %q (supported: ts)", lang)
			}
			ont, diags := compileDir(dirArg(args))
			if diags.HasErrors(false) {
				if err := printDiagnostics(stderr, diags, "text"); err != nil {
					return err
				}
				return errFailed
			}
			ont.Version = version
			src, err := codegen.TypeScript(ont, codegen.Options{Header: header})
			if err != nil {
				return err
			}
			if out == "" || out == "-" {
				_, err = stdout.Write(src)
				return err
			}
			if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(out, src, 0o644); err != nil {
				return err
			}
			fmt.Fprintf(stderr, "wrote %s (%d bytes)\n", out, len(src))
			return nil
		},
	}
	cmd.Flags().StringVar(&lang, "lang", "ts", "target language (ts)")
	cmd.Flags().StringVarP(&out, "out", "o", "", "output file (default stdout)")
	cmd.Flags().StringVar(&header, "header", "", "extra comment line, e.g. the source commit")
	cmd.Flags().Int64Var(&version, "ontology-version", 0, "OMS version the SDK targets (recorded as ONTOLOGY_VERSION)")
	return cmd
}
