package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/acme/ontology/internal/authz"
)

func newFGACmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{Use: "fga", Short: "Manage the OpenFGA authorization model"}
	cmd.AddCommand(newFGACheckCmd(stdout, stderr), newFGAInitCmd(stdout, stderr))
	return cmd
}

func loadModel(path string) (*authz.Model, []byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	m, err := authz.ParseModel(raw)
	return m, raw, err
}

// checkAgainstModel compiles the ontology and reports mismatches with the model.
func checkAgainstModel(dir, modelPath string, stdout, stderr io.Writer) error {
	ont, diags := compileDir(dir)
	if diags.HasErrors(false) {
		if err := printDiagnostics(stderr, diags, "text"); err != nil {
			return err
		}
		return errFailed
	}
	m, _, err := loadModel(modelPath)
	if err != nil {
		return err
	}
	problems := authz.CheckModel(ont, m)
	for _, p := range problems {
		fmt.Fprintln(stderr, "FGA001", p.String())
	}
	if len(problems) > 0 {
		fmt.Fprintf(stderr, "%d mismatch(es) between %s and %s\n", len(problems), dir, modelPath)
		return errFailed
	}
	fmt.Fprintf(stdout, "ontology %s is consistent with %s\n", ont.APIName, modelPath)
	return nil
}

func newFGACheckCmd(stdout, stderr io.Writer) *cobra.Command {
	var modelPath string
	cmd := &cobra.Command{
		Use:   "check [dir]",
		Short: "Check that authz settings in the ontology match the authorization model (offline)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return checkAgainstModel(dirArg(args), modelPath, stdout, stderr)
		},
	}
	cmd.Flags().StringVar(&modelPath, "model", "deploy/openfga/model.json", "authorization model (OpenFGA JSON syntax)")
	return cmd
}

func newFGAInitCmd(stdout, stderr io.Writer) *cobra.Command {
	var modelPath, apiURL, storeName, tuplesPath string
	cmd := &cobra.Command{
		Use:   "init [dir]",
		Short: "Create the store if needed, upload the model if it changed, and print the IDs (idempotent)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkAgainstModel(dirArg(args), modelPath, io.Discard, stderr); err != nil {
				return err
			}
			if apiURL == "" {
				apiURL = os.Getenv("ONTO_FGA_URL")
			}
			if apiURL == "" {
				return fmt.Errorf("--fga-url (or ONTO_FGA_URL) is required")
			}
			_, raw, err := loadModel(modelPath)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), time.Minute)
			defer cancel()
			admin := &authz.Admin{APIURL: apiURL, Token: os.Getenv("ONTO_FGA_TOKEN")}
			store, created, err := admin.EnsureStore(ctx, storeName)
			if err != nil {
				return err
			}
			modelID, wrote, err := admin.EnsureModel(ctx, store.ID, raw)
			if err != nil {
				return err
			}
			if tuplesPath != "" {
				b, err := os.ReadFile(tuplesPath)
				if err != nil {
					return err
				}
				var seed struct {
					Tuples []authz.Tuple `json:"tuples"`
				}
				if err := json.Unmarshal(b, &seed); err != nil {
					return fmt.Errorf("%s: %w", tuplesPath, err)
				}
				fga := authz.NewOpenFGA(apiURL, store.ID, modelID)
				if err := fga.WriteTuples(ctx, seed.Tuples, nil); err != nil {
					return fmt.Errorf("seed tuples: %w", err)
				}
				fmt.Fprintf(stderr, "seeded %d tuple(s) from %s\n", len(seed.Tuples), tuplesPath)
			}
			fmt.Fprintf(stderr, "store %q %s; model %s\n", storeName, map[bool]string{true: "created", false: "exists"}[created],
				map[bool]string{true: "uploaded", false: "unchanged"}[wrote])
			// stdout is shell-friendly: eval "$(ontoctl fga init)"
			fmt.Fprintf(stdout, "export ONTO_FGA_URL=%s\nexport ONTO_FGA_STORE_ID=%s\nexport ONTO_FGA_MODEL_ID=%s\n", apiURL, store.ID, modelID)
			return nil
		},
	}
	cmd.Flags().StringVar(&modelPath, "model", "deploy/openfga/model.json", "authorization model (OpenFGA JSON syntax)")
	cmd.Flags().StringVar(&apiURL, "fga-url", "", "OpenFGA API URL (default $ONTO_FGA_URL)")
	cmd.Flags().StringVar(&storeName, "store", "ontology", "store name")
	cmd.Flags().StringVar(&tuplesPath, "seed", "", "optional JSON file {\"tuples\":[...]} to write after the model (dev data)")
	return cmd
}
