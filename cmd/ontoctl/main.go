// Command ontoctl validates and (later) publishes Ontology definitions.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	_ "github.com/acme/ontology/internal/expr" // registers CEL validators with spec via init
	"github.com/acme/ontology/internal/objstore"
	"github.com/acme/ontology/internal/spec"
)

var version = "dev"

// errFailed signals exit code 1 after diagnostics were already printed.
var errFailed = errors.New("validation failed")

func main() {
	if err := newRootCmd(os.Stdout, os.Stderr).Execute(); err != nil {
		if !errors.Is(err, errFailed) {
			fmt.Fprintln(os.Stderr, "error:", err)
		}
		os.Exit(1)
	}
}

func newRootCmd(stdout, stderr io.Writer) *cobra.Command {
	root := &cobra.Command{
		Use:           "ontoctl",
		Short:         "Ontology definition tooling",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.AddCommand(
		newValidateCmd(stdout),
		newExportCmd(stdout, stderr),
		newDDLCmd(stdout, stderr),
		newDiffCmd(stdout, stderr),
		newPublishCmd(stdout, stderr),
		newFGACmd(stdout, stderr),
		&cobra.Command{
			Use:   "version",
			Short: "Print version",
			Run:   func(*cobra.Command, []string) { fmt.Fprintln(stdout, version) },
		},
	)
	return root
}

func dirArg(args []string) string {
	if len(args) == 1 {
		return args[0]
	}
	return "ontology"
}

func compileDir(dir string) (*spec.CompiledOntology, spec.Diagnostics) {
	files, diags := spec.LoadFS(os.DirFS(dir), filepath.ToSlash(filepath.Clean(dir)))
	ont, cdiags := spec.Compile(files)
	all := append(diags, cdiags...)
	all.Sort()
	return ont, all
}

func newValidateCmd(out io.Writer) *cobra.Command {
	var format string
	var strict bool
	cmd := &cobra.Command{
		Use:   "validate [dir]",
		Short: "Compile and validate ontology YAML (exit 1 on errors)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			_, diags := compileDir(dirArg(args))
			if err := printDiagnostics(out, diags, format); err != nil {
				return err
			}
			if diags.HasErrors(strict) {
				return errFailed
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&format, "format", "text", "output format: text|json")
	cmd.Flags().BoolVar(&strict, "strict", false, "treat warnings as errors")
	return cmd
}

func newExportCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "export [dir]",
		Short: "Print the compiled ontology as JSON",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			ont, diags := compileDir(dirArg(args))
			if diags.HasErrors(false) {
				if err := printDiagnostics(stderr, diags, "text"); err != nil {
					return err
				}
				return errFailed
			}
			enc := json.NewEncoder(stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(ont)
		},
	}
	return cmd
}

func newDDLCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "ddl [dir]",
		Short: "Print idempotent PostgreSQL DDL for the object tables",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			ont, diags := compileDir(dirArg(args))
			if diags.HasErrors(false) {
				if err := printDiagnostics(stderr, diags, "text"); err != nil {
					return err
				}
				return errFailed
			}
			stmts, err := objstore.GenerateDDL(ont)
			if err != nil {
				return err
			}
			for _, st := range stmts {
				fmt.Fprintln(stdout, st+";")
			}
			return nil
		},
	}
}

func newDiffCmd(stdout, stderr io.Writer) *cobra.Command {
	var against string
	var format string
	var allowTighten bool
	cmd := &cobra.Command{
		Use:   "diff [dir]",
		Short: "Diff an ontology against a previous compiled JSON (--against file.json)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			newOnt, diags := compileDir(dirArg(args))
			if diags.HasErrors(false) {
				if err := printDiagnostics(stderr, diags, "text"); err != nil {
					return err
				}
				return errFailed
			}
			var oldOnt *spec.CompiledOntology
			if against != "" {
				data, err := os.ReadFile(against)
				if err != nil {
					return fmt.Errorf("read --against: %w", err)
				}
				oldOnt = &spec.CompiledOntology{}
				if err := json.Unmarshal(data, oldOnt); err != nil {
					return fmt.Errorf("parse --against JSON: %w", err)
				}
			}
			cs := spec.Diff(oldOnt, newOnt)
			missing := spec.MigrationCoverage(cs, newOnt.Migrations)
			if err := printChangeSet(stdout, cs, missing, format); err != nil {
				return err
			}
			// PUB002: breaking changes without migrations fail the command (CI gate).
			blocking := 0
			for _, c := range missing {
				if c.Kind == spec.TightenConstraint && allowTighten {
					continue
				}
				blocking++
			}
			if blocking > 0 {
				return errFailed
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&against, "against", "", "path to a previously compiled ontology JSON (from `ontoctl export`)")
	cmd.Flags().StringVar(&format, "format", "text", "output format: text|json|markdown")
	cmd.Flags().BoolVar(&allowTighten, "allow-constraint-tightening", false, "do not fail on tightened constraints")
	return cmd
}

func printChangeSet(w io.Writer, cs spec.ChangeSet, missing []spec.Change, format string) error {
	switch format {
	case "json":
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(struct {
			Changes        []spec.Change `json:"changes"`
			MissingMigrate []spec.Change `json:"missingMigrations"`
		}{cs.Changes, missing})
	case "markdown":
		fmt.Fprintln(w, "| Change | Path | Breaking |")
		fmt.Fprintln(w, "|---|---|---|")
		for _, c := range cs.Changes {
			fmt.Fprintf(w, "| %s | %s | %t |\n", c.Kind, c.Path, c.Breaking)
		}
		if len(missing) > 0 {
			fmt.Fprintln(w, "\n**Breaking changes missing a migration:**")
			for _, c := range missing {
				fmt.Fprintf(w, "- %s %s: %s\n", c.Kind, c.Path, c.Detail)
			}
		}
		return nil
	case "text":
		for _, c := range cs.Changes {
			tag := " "
			if c.Breaking {
				tag = "!"
			}
			fmt.Fprintf(w, "[%s] %s %s", tag, c.Kind, c.Path)
			if c.Detail != "" {
				fmt.Fprintf(w, " — %s", c.Detail)
			}
			fmt.Fprintln(w)
		}
		if len(missing) > 0 {
			fmt.Fprintf(w, "\n%d breaking change(s) without a migration:\n", len(missing))
			for _, c := range missing {
				fmt.Fprintf(w, "  PUB002 %s %s\n", c.Kind, c.Path)
			}
		}
		return nil
	default:
		return fmt.Errorf("unsupported format %q (want text|json|markdown)", format)
	}
}

func printDiagnostics(w io.Writer, diags spec.Diagnostics, format string) error {
	switch format {
	case "text":
		for _, d := range diags {
			fmt.Fprintln(w, d.String())
		}
		e, wn := diags.Count()
		fmt.Fprintf(w, "%d error(s), %d warning(s)\n", e, wn)
		return nil
	case "json":
		if diags == nil {
			diags = spec.Diagnostics{}
		}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(struct {
			Diagnostics spec.Diagnostics `json:"diagnostics"`
		}{diags})
	default:
		return fmt.Errorf("unsupported format %q (want text|json)", format)
	}
}
