package load_test

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-quicktest/qt"
	"golang.org/x/tools/txtar"

	"cuelang.org/go/cue/build"
	"cuelang.org/go/cue/cuecontext"
	"cuelang.org/go/cue/errors"
	"cuelang.org/go/cue/load"
	"cuelang.org/go/internal/cuetxtar"
	"cuelang.org/go/mod/modcache"
	"cuelang.org/go/mod/modregistrytest"
)

func TestModuleLoadWithInvalidRegistryConfig(t *testing.T) {
	// When there's an invalid registry configuration,
	// we shouldn't get an error unless the module actually tries to use a registry.
	t.Setenv("CUE_REGISTRY", "invalid}host:")
	cacheDir := t.TempDir()
	t.Setenv("CUE_CACHE_DIR", cacheDir)

	insts := load.Instances([]string{"./imports"}, &load.Config{
		Dir: filepath.Join("testdata", "testmod"),
	})
	qt.Assert(t, qt.IsNil(insts[0].Err))

	// Check that nothing has been created in the cache directory (no
	// side-effects).
	entries, err := os.ReadDir(cacheDir)
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.HasLen(entries, 0))

	// Now check that we do get an error when we try to use a module
	// that requires module resolution.
	testData, err := os.ReadFile(filepath.Join("testdata", "testfetch", "simple.txtar"))
	qt.Assert(t, qt.IsNil(err))

	cfg := &load.Config{
		Dir:     t.TempDir(),
		Overlay: map[string]load.Source{},
	}
	a := txtar.Parse(testData)
	for _, f := range a.Files {
		if !strings.HasPrefix(f.Name, "_registry/") {
			cfg.Overlay[filepath.Join(cfg.Dir, f.Name)] = load.FromBytes(f.Data)
		}
	}
	insts = load.Instances([]string{"."}, cfg)
	qt.Assert(t, qt.ErrorMatches(insts[0].Err, `import failed: .*main.cue:2:8: cannot find package "example.com@v0": cannot fetch example.com@v0.0.1: bad value for registry: invalid registry "invalid}host:": invalid host name "invalid}host:" in registry`))

	// Try again with environment variables passed in Env.
	// This is really just a smoke test to make sure that Env is
	// passed through to the underlying modconfig call.
	cfg.Env = []string{
		"CUE_REGISTRY=invalid}host2:",
		"CUE_CACHE_DIR=" + cacheDir,
	}
	insts = load.Instances([]string{"."}, cfg)
	qt.Assert(t, qt.ErrorMatches(insts[0].Err, `import failed: .*main.cue:2:8: cannot find package "example.com@v0": cannot fetch example.com@v0.0.1: bad value for registry: invalid registry "invalid}host2:": invalid host name "invalid}host2:" in registry`))
}

func TestModuleFetch(t *testing.T) {
	test := cuetxtar.TxTarTest{
		Root: "./testdata/testfetch",
		Name: "modfetch",
	}
	test.Run(t, func(t *cuetxtar.Test) {
		tfs, err := txtar.FS(t.Archive)
		qt.Assert(t, qt.IsNil(err))
		rfs, err := fs.Sub(tfs, "_registry")
		qt.Assert(t, qt.IsNil(err))
		r, err := modregistrytest.New(rfs, "")
		qt.Assert(t, qt.IsNil(err))
		defer r.Close()

		tmpDir := t.TempDir()
		t.LoadConfig.Env = []string{
			"CUE_CACHE_DIR=" + filepath.Join(tmpDir, "cache"),
			"CUE_REGISTRY=" + r.Host() + "+insecure",
			"CUE_CONFIG_DIR=" + filepath.Join(tmpDir, "config"),
		}
		// The fetched files are read-only, so testing fails when trying
		// to remove them.
		defer modcache.RemoveAll(tmpDir)
		ctx := cuecontext.New()
		// Note: avoid calling RawInstances with no arguments, because
		// that loads the top level CUE files directly rather than just
		// loading the package which is what we want.
		insts := t.RawInstances(".")
		if len(insts) != 1 {
			t.Fatalf("wrong instance count; got %d want 1", len(insts))
		}
		inst := insts[0]
		if inst.Err != nil {
			errors.Print(t.Writer("error"), inst.Err, &errors.Config{
				ToSlash: true,
				Cwd:     t.Dir,
			})
			return
		}
		writeInstanceInfo(t.T, t.Writer("instance-info"), inst, 0)
		qt.Assert(t, qt.Not(qt.IsNil(inst.ModuleFile)))
		qt.Assert(t, qt.Equals(inst.ModuleFile.QualifiedModule(), inst.Module))
		qt.Assert(t, qt.Equals(inst.ModuleFile.Language.Version, "v0.9.0-main"))
		v := ctx.BuildInstance(inst)
		if err := v.Validate(); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(t.Writer("result"), "%v\n", v)
	})
}

func writeInstanceInfo(t *testing.T, w io.Writer, inst *build.Instance, depth int) {
	m := inst.Module
	versionStr := ""
	if m == "" {
		m = "<none>"
		qt.Assert(t, qt.IsNil(inst.ModuleFile))
	} else {
		qt.Assert(t, qt.Not(qt.IsNil(inst.ModuleFile)))
		qt.Assert(t, qt.Equals(inst.ModuleFile.QualifiedModule(), inst.Module))
		versionStr = fmt.Sprintf(" version=%v", inst.ModuleFile.Language.Version)
	}
	errStr := ""
	if inst.Err != nil {
		errStr = fmt.Sprintf(" err=%v", inst.Err)
	}
	fmt.Fprintf(w, "%s%s module=%s%s%s\n", strings.Repeat("\t", depth), inst.ImportPath, m, versionStr, errStr)
	for _, inst := range inst.Imports {
		writeInstanceInfo(t, w, inst, depth+1)
	}

	// Check that the source files have actually been parsed with
	// the expected version.
	for _, f := range inst.Files {
		qt.Check(t, qt.Equals(f.LanguageVersion, inst.ModuleFile.Language.Version), qt.Commentf("pkg %v", inst.ImportPath))
	}
}

func TestExplicitCUEFilesGetModuleVersion(t *testing.T) {
	// When a file isn't part of a module, it will be parsed according
	// to the current module version if there is one.

	tfs, err := txtar.FS(txtar.Parse([]byte(`
-- something.cue --
package something
-- mod/cue.mod/module.cue --
module: "foo.test"
language: version: "v0.9.0-test"
-- mod/main.cue --
package main
`)))
	qt.Assert(t, qt.IsNil(err))
	dir := t.TempDir()
	err = os.CopyFS(dir, tfs)
	qt.Assert(t, qt.IsNil(err))
	insts := load.Instances([]string{"../something.cue"}, &load.Config{
		Dir: filepath.Join(dir, "mod"),
	})
	qt.Assert(t, qt.HasLen(insts, 1))
	qt.Assert(t, qt.HasLen(insts[0].Files, 1))
	qt.Assert(t, qt.Equals(insts[0].Files[0].LanguageVersion, "v0.9.0-test"))
}
