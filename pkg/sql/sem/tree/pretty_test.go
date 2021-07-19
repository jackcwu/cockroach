// Copyright 2018 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package tree_test

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io/ioutil"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/sql/parser"
	_ "github.com/cockroachdb/cockroach/pkg/sql/sem/builtins"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/cockroachdb/cockroach/pkg/testutils/sqlutils"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/pretty"
	"golang.org/x/sync/errgroup"
)

var (
	flagWritePretty = flag.Bool("rewrite-pretty", false, "rewrite pretty test outputs")
	testPrettyCfg   = func() tree.PrettyCfg {
		cfg := tree.DefaultPrettyCfg()
		cfg.JSONFmt = true
		return cfg
	}()
)

// TestPrettyData reads in a single SQL statement from a file, formats
// it at 40 characters width, and compares that output to a known-good
// output file. It is most useful when changing or implementing the
// doc interface for a node, and should be used to compare and verify
// the changed output.
func TestPrettyDataShort(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	matches, err := filepath.Glob(filepath.Join("testdata", "pretty", "*.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if *flagWritePretty {
		t.Log("WARNING: do not forget to run TestPrettyData with build flag 'nightly' and the -rewrite-pretty flag too!")
	}
	cfg := testPrettyCfg
	cfg.Align = tree.PrettyNoAlign
	t.Run("ref", func(t *testing.T) {
		runTestPrettyData(t, "ref", cfg, matches, true /*short*/)
	})
	cfg.Align = tree.PrettyAlignAndDeindent
	t.Run("align-deindent", func(t *testing.T) {
		runTestPrettyData(t, "align-deindent", cfg, matches, true /*short*/)
	})
	cfg.Align = tree.PrettyAlignOnly
	t.Run("align-only", func(t *testing.T) {
		runTestPrettyData(t, "align-only", cfg, matches, true /*short*/)
	})
}

func runTestPrettyData(
	t *testing.T, prefix string, cfg tree.PrettyCfg, matches []string, short bool,
) {
	for _, m := range matches {
		m := m
		t.Run(filepath.Base(m), func(t *testing.T) {
			sql, err := ioutil.ReadFile(m)
			if err != nil {
				t.Fatal(err)
			}
			stmt, err := parser.ParseOne(string(sql))
			if err != nil {
				t.Fatal(err)
			}

			// We have a statement, now we need to format it at all possible line
			// lengths. We use the length of the string + 10 as the upper bound to try to
			// find what happens at the longest line length. Preallocate a result slice and
			// work chan, then fire off a bunch of workers to compute all of the variants.
			var res []string
			if short {
				res = []string{""}
			} else {
				res = make([]string, len(sql)+10)
			}
			type param struct{ idx, numCols int }
			work := make(chan param, len(res))
			if short {
				work <- param{0, 40}
			} else {
				for i := range res {
					work <- param{i, i + 1}
				}
			}
			close(work)
			g, _ := errgroup.WithContext(context.Background())
			worker := func() error {
				for p := range work {
					thisCfg := cfg
					thisCfg.LineWidth = p.numCols
					res[p.idx] = thisCfg.Pretty(stmt.AST)
				}
				return nil
			}
			for i := 0; i < runtime.NumCPU(); i++ {
				g.Go(worker)
			}
			if err := g.Wait(); err != nil {
				t.Fatal(err)
			}
			var sb strings.Builder
			for i, s := range res {
				// Only write each new result to the output, along with a small header
				// indicating the line length.
				if i == 0 || s != res[i-1] {
					fmt.Fprintf(&sb, "%d:\n%s\n%s\n\n", i+1, strings.Repeat("-", i+1), s)
				}
			}
			var gotB bytes.Buffer
			gotB.WriteString("// Code generated by TestPretty. DO NOT EDIT.\n")
			gotB.WriteString("// GENERATED FILE DO NOT EDIT\n")
			gotB.WriteString(sb.String())
			gotB.WriteByte('\n')
			got := gotB.String()

			ext := filepath.Ext(m)
			outfile := m[:len(m)-len(ext)] + "." + prefix + ".golden"
			if short {
				outfile = outfile + ".short"
			}

			if *flagWritePretty {
				if err := ioutil.WriteFile(outfile, []byte(got), 0666); err != nil {
					t.Fatal(err)
				}
				return
			}

			expect, err := ioutil.ReadFile(outfile)
			if err != nil {
				t.Fatal(err)
			}
			if string(expect) != got {
				t.Fatalf("expected:\n%s\ngot:\n%s", expect, got)
			}

			sqlutils.VerifyStatementPrettyRoundtrip(t, string(sql))
		})
	}
}

func TestPrettyVerify(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	tests := map[string]string{
		// Verify that INTERVAL is maintained.
		`SELECT interval '-2µs'`: `SELECT '-00:00:00.000002':::INTERVAL`,
	}
	for orig, pretty := range tests {
		t.Run(orig, func(t *testing.T) {
			sqlutils.VerifyStatementPrettyRoundtrip(t, orig)

			stmt, err := parser.ParseOne(orig)
			if err != nil {
				t.Fatal(err)
			}
			got := tree.Pretty(stmt.AST)
			if pretty != got {
				t.Fatalf("got: %s\nexpected: %s", got, pretty)
			}
		})
	}
}

func BenchmarkPrettyData(b *testing.B) {
	matches, err := filepath.Glob(filepath.Join("testdata", "pretty", "*.sql"))
	if err != nil {
		b.Fatal(err)
	}
	var docs []pretty.Doc
	cfg := tree.DefaultPrettyCfg()
	for _, m := range matches {
		sql, err := ioutil.ReadFile(m)
		if err != nil {
			b.Fatal(err)
		}
		stmt, err := parser.ParseOne(string(sql))
		if err != nil {
			b.Fatal(err)
		}
		docs = append(docs, cfg.Doc(stmt.AST))
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, doc := range docs {
			for _, w := range []int{1, 30, 80} {
				pretty.Pretty(doc, w, true /*useTabs*/, 4 /*tabWidth*/, nil /* keywordTransform */)
			}
		}
	}
}

func TestPrettyExprs(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	tests := map[tree.Expr]string{
		&tree.CastExpr{
			Expr: tree.NewDString("foo"),
			Type: types.MakeCollatedString(types.String, "en"),
		}: `CAST('foo':::STRING AS STRING) COLLATE en`,
	}

	for expr, pretty := range tests {
		got := tree.Pretty(expr)
		if pretty != got {
			t.Fatalf("got: %s\nexpected: %s", got, pretty)
		}
	}
}
