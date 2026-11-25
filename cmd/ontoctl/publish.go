package main

import (
    "bytes"
    "context"
    "encoding/json"
    "fmt"
    "io"
    "io/fs"
    "net/http"
    "os"
    "path"
    "sort"
    "strings"
    "time"

    "github.com/spf13/cobra"

    "github.com/acme/ontology/internal/spec"
)

// maxPublishBytes bounds the total size of sources sent to OMS.
const maxPublishBytes = 8 << 20

type sourceFile struct {
    Path    string `json:"path"`
    Content string `json:"content"`
}

type publishRequest struct {
    GitCommit    string       `json:"gitCommit"`
    Files        []sourceFile `json:"files"`
    DryRun       bool         `json:"dryRun"`
    AllowTighten bool         `json:"allowConstraintTightening"`
}

type publishResponse struct {
    Version     int64            `json:"version"`
    Status      string           `json:"status"`
    ChangeSet   spec.ChangeSet   `json:"changeset"`
    Diagnostics spec.Diagnostics `json:"diagnostics"`
    Reused      bool             `json:"reused"`
}

type omsError struct {
    ErrorCode string         `json:"errorCode"`
    Message   string         `json:"message"`
    Details   map[string]any `json:"details"`
}

// readSources collects *.yaml / *.yml under dir with slash-separated relative paths, sorted.
func readSources(dir string) ([]sourceFile, error) {
    fsys := os.DirFS(dir)
    var out []sourceFile
    total := 0
    err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
        if err != nil {
            return err
        }
        if d.IsDir() {
            return nil
        }
        switch strings.ToLower(path.Ext(p)) {
        case ".yaml", ".yml":
        default:
            return nil
        }
        b, err := fs.ReadFile(fsys, p)
        if err != nil {
            return err
        }
        total += len(b)
        if total > maxPublishBytes {
            return fmt.Errorf("ontology sources exceed %d bytes", maxPublishBytes)
        }
        out = append(out, sourceFile{Path: p, Content: string(b)})
        return nil
    })
    sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
    return out, err
}

func newPublishCmd(stdout, stderr io.Writer) *cobra.Command {
    var omsURL, gitCommit, actor, format string
    var dryRun, allowTighten, skipLocal bool
    var timeout time.Duration
    cmd := &cobra.Command{
        Use:   "publish [dir]",
        Short: "Validate locally, then publish the ontology to OMS (exit 1 on rejection)",
        Args:  cobra.MaximumNArgs(1),
        RunE: func(cmd *cobra.Command, args []string) error {
            dir := dirArg(args)
            if omsURL == "" {
                omsURL = os.Getenv("ONTO_OMS_URL")
            }
            if omsURL == "" {
                return fmt.Errorf("--oms (or ONTO_OMS_URL) is required")
            }
            if gitCommit == "" {
                gitCommit = os.Getenv("GITHUB_SHA")
            }
            if gitCommit == "" && !dryRun {
                return fmt.Errorf("--git-commit is required (or set GITHUB_SHA)")
            }

            // 1. local validation first: fast feedback, no network round trip for obvious errors
            ont, diags := compileDir(dir)
            if !skipLocal && diags.HasErrors(false) {
                if err := printDiagnostics(stderr, diags, "text"); err != nil {
                    return err
                }
                return errFailed
            }

            files, err := readSources(dir)
            if err != nil {
                return err
            }
            if len(files) == 0 {
                return fmt.Errorf("no YAML files found in %s", dir)
            }

            body, err := json.Marshal(publishRequest{GitCommit: gitCommit, Files: files, DryRun: dryRun, AllowTighten: allowTighten})
            if err != nil {
                return err
            }
            ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
            defer cancel()
            url := fmt.Sprintf("%s/api/v1/ontologies/%s/versions", strings.TrimRight(omsURL, "/"), ont.APIName)
            req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
            if err != nil {
                return err
            }
            req.Header.Set("Content-Type", "application/json")
            if tok := os.Getenv("ONTO_TOKEN"); tok != "" {
                req.Header.Set("Authorization", "Bearer "+tok)
            }
            if actor == "" {
                actor = os.Getenv("ONTO_ACTOR")
            }
            if actor != "" {
                req.Header.Set("X-Actor", actor) // dev OMS identity; production uses the bearer token
            }
            resp, err := http.DefaultClient.Do(req)
            if err != nil {
                return fmt.Errorf("publish request failed: %w", err)
            }
            defer resp.Body.Close()
            raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))

            if resp.StatusCode >= 300 {
                var oe omsError
                if json.Unmarshal(raw, &oe) != nil || oe.ErrorCode == "" {
                    return fmt.Errorf("OMS returned %s: %s", resp.Status, strings.TrimSpace(string(raw)))
                }
                printPublishError(stderr, oe, format)
                return errFailed
            }
            var pr publishResponse
            if err := json.Unmarshal(raw, &pr); err != nil {
                return fmt.Errorf("decode OMS response: %w", err)
            }
            return printPublishResult(stdout, ont.APIName, pr, format)
        },
    }
    cmd.Flags().StringVar(&omsURL, "oms", "", "OMS base URL (default $ONTO_OMS_URL)")
    cmd.Flags().StringVar(&gitCommit, "git-commit", "", "commit SHA recorded with the version (default $GITHUB_SHA)")
    cmd.Flags().StringVar(&actor, "actor", "", "publisher identity for dev OMS (default $ONTO_ACTOR)")
    cmd.Flags().BoolVar(&dryRun, "dry-run", false, "validate and diff on the server without creating a version")
    cmd.Flags().BoolVar(&allowTighten, "allow-constraint-tightening", false, "do not reject tightened constraints")
    cmd.Flags().BoolVar(&skipLocal, "skip-local-validation", false, "send even if local validation fails (server still validates)")
    cmd.Flags().StringVar(&format, "format", "text", "output format: text|json")
    cmd.Flags().DurationVar(&timeout, "timeout", 2*time.Minute, "request timeout (publishing applies DDL)")
    return cmd
}

func printPublishResult(w io.Writer, ontology string, pr publishResponse, format string) error {
    if format == "json" {
        enc := json.NewEncoder(w)
        enc.SetIndent("", "  ")
        return enc.Encode(pr)
    }
    switch {
    case pr.Status == "DRY_RUN":
        fmt.Fprintf(w, "dry run: %s would change as follows (current active version %d)\n", ontology, pr.Version)
    case pr.Reused:
        fmt.Fprintf(w, "unchanged: %s version %d already has this content (%s)\n", ontology, pr.Version, pr.Status)
    default:
        fmt.Fprintf(w, "published %s version %d (%s)\n", ontology, pr.Version, pr.Status)
    }
    for _, c := range pr.ChangeSet.Changes {
        tag := " "
        if c.Breaking {
            tag = "!"
        }
        fmt.Fprintf(w, "  [%s] %s %s\n", tag, c.Kind, c.Path)
    }
    return nil
}

func printPublishError(w io.Writer, oe omsError, format string) {
    if format == "json" {
        enc := json.NewEncoder(w)
        enc.SetIndent("", "  ")
        _ = enc.Encode(oe)
        return
    }
    fmt.Fprintf(w, "publish rejected: %s: %s\n", oe.ErrorCode, oe.Message)
    for _, key := range []string{"diagnostics", "missingMigrations"} {
        items, ok := oe.Details[key].([]any)
        if !ok {
            continue
        }
        for _, it := range items {
            m, _ := it.(map[string]any)
            switch key {
            case "diagnostics":
                fmt.Fprintf(w, "  %v %v: %v\n", m["code"], m["path"], m["message"])
            default:
                fmt.Fprintf(w, "  PUB002 %v %v %v\n", m["kind"], m["path"], m["detail"])
            }
        }
    }
}
