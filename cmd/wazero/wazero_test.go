package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental/logging"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/tetratelabs/wazero/internal/internalapi"
	"github.com/tetratelabs/wazero/internal/platform"
	"github.com/tetratelabs/wazero/internal/testing/require"
	"github.com/tetratelabs/wazero/internal/version"
	"github.com/tetratelabs/wazero/sys"
)

//go:embed testdata/infinite_loop.wasm
var wasmInfiniteLoop []byte

//go:embed testdata/wasi_arg.wasm
var wasmWasiArg []byte

//go:embed testdata/wasi_env.wasm
var wasmWasiEnv []byte

//go:embed testdata/wasi_fd.wasm
var wasmWasiFd []byte

//go:embed testdata/wasi_random_get.wasm
var wasmWasiRandomGet []byte

//go:embed testdata/cat/cat-tinygo.wasm
var wasmCatTinygo []byte

//go:embed testdata/exit_on_start_unstable.wasm
var wasmWasiUnstable []byte

// The canonical debug-built Rust fixture compiled to `wasm32-wasip1`
// with full DWARF is no longer embedded here.  Its source lives at
// `test_code/rust_test.rs` — the program declares `add_3_and_4` (which
// returns 7) and `main`, plus three local let-bindings (`blq` = "abcd",
// `x` = 3, `y` = 4) and one Sample struct — and it is compiled during
// the test run by `rustTestFixture` (see `rust_fixture_test.go`).  Used
// by `TestRecordedTraceViaCtPrintJson` to assert exact decoded values
// via `ct-print --full --strip-paths`.

func TestMain(m *testing.M) {
	cmd := exec.Command("go", "version")
	if _, err := cmd.CombinedOutput(); err != nil {
		log.Println("main: cli test is only supported on a machine with Go installed")
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestCompile(t *testing.T) {
	tmpDir, oldwd := requireChdirToTemp(t)
	defer os.Chdir(oldwd) //nolint

	wasmPath := filepath.Join(tmpDir, "test.wasm")
	require.NoError(t, os.WriteFile(wasmPath, wasmWasiArg, 0o600))

	existingDir1 := filepath.Join(tmpDir, "existing1")
	require.NoError(t, os.Mkdir(existingDir1, 0o700))
	existingDir2 := filepath.Join(tmpDir, "existing2")
	require.NoError(t, os.Mkdir(existingDir2, 0o700))

	cpuProfile := filepath.Join(t.TempDir(), "cpu.out")
	memProfile := filepath.Join(t.TempDir(), "mem.out")

	tests := []struct {
		name       string
		wazeroOpts []string
		test       func(t *testing.T)
	}{
		{
			name: "no opts",
		},
		{
			name:       "cachedir existing absolute",
			wazeroOpts: []string{"--cachedir=" + existingDir1},
			test: func(t *testing.T) {
				entries, err := os.ReadDir(existingDir1)
				require.NoError(t, err)
				require.True(t, len(entries) > 0)
			},
		},
		{
			name:       "cachedir existing relative",
			wazeroOpts: []string{"--cachedir=existing2"},
			test: func(t *testing.T) {
				entries, err := os.ReadDir(existingDir2)
				require.NoError(t, err)
				require.True(t, len(entries) > 0)
			},
		},
		{
			name:       "cachedir new absolute",
			wazeroOpts: []string{"--cachedir=" + path.Join(tmpDir, "new1")},
			test: func(t *testing.T) {
				entries, err := os.ReadDir("new1")
				require.NoError(t, err)
				require.True(t, len(entries) > 0)
			},
		},
		{
			name:       "cachedir new relative",
			wazeroOpts: []string{"--cachedir=new2"},
			test: func(t *testing.T) {
				entries, err := os.ReadDir("new2")
				require.NoError(t, err)
				require.True(t, len(entries) > 0)
			},
		},
		{
			name:       "enable cpu profiling",
			wazeroOpts: []string{"-cpuprofile=" + cpuProfile},
			test: func(t *testing.T) {
				require.NoError(t, exist(cpuProfile))
			},
		},
		{
			name:       "enable memory profiling",
			wazeroOpts: []string{"-memprofile=" + memProfile},
			test: func(t *testing.T) {
				require.NoError(t, exist(memProfile))
			},
		},
	}

	for _, tc := range tests {
		tt := tc
		t.Run(tt.name, func(t *testing.T) {
			args := append([]string{"compile"}, tt.wazeroOpts...)
			args = append(args, wasmPath)
			exitCode, stdout, stderr := runMain(t, "", args)
			require.Zero(t, stderr)
			require.Equal(t, 0, exitCode, stderr)
			require.Zero(t, stdout)
			if test := tt.test; test != nil {
				test(t)
			}
		})
	}
}

func requireChdirToTemp(t *testing.T) (string, string) {
	tmpDir := t.TempDir()
	oldwd, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(tmpDir))
	return tmpDir, oldwd
}

func TestCompile_Errors(t *testing.T) {
	tmpDir := t.TempDir()

	wasmPath := filepath.Join(tmpDir, "test.wasm")
	require.NoError(t, os.WriteFile(wasmPath, wasmWasiArg, 0o600))

	notWasmPath := filepath.Join(tmpDir, "bears.wasm")
	require.NoError(t, os.WriteFile(notWasmPath, []byte("pooh"), 0o600))

	tests := []struct {
		message string
		args    []string
	}{
		{
			message: "missing path to wasm file",
			args:    []string{},
		},
		{
			message: "error reading wasm binary",
			args:    []string{"non-existent.wasm"},
		},
		{
			message: "error compiling wasm binary",
			args:    []string{notWasmPath},
		},
		{
			message: "invalid cachedir",
			args:    []string{"--cachedir", notWasmPath, wasmPath},
		},
	}

	for _, tc := range tests {
		tt := tc
		t.Run(tt.message, func(t *testing.T) {
			exitCode, _, stderr := runMain(t, "", append([]string{"compile"}, tt.args...))

			require.Equal(t, 1, exitCode)
			require.Contains(t, stderr, tt.message)
		})
	}
}

func TestRun(t *testing.T) {
	tmpDir, oldwd := requireChdirToTemp(t)
	defer os.Chdir(oldwd) //nolint

	// Restore env logic borrowed from TestClearenv
	defer func(origEnv []string) {
		for _, pair := range origEnv {
			// Environment variables on Windows can begin with =
			// https://blogs.msdn.com/b/oldnewthing/archive/2010/05/06/10008132.aspx
			i := strings.Index(pair[1:], "=") + 1
			if err := os.Setenv(pair[:i], pair[i+1:]); err != nil {
				t.Errorf("Setenv(%q, %q) failed during reset: %v", pair[:i], pair[i+1:], err)
			}
		}
	}(os.Environ())

	// Clear the environment first, so we can make strict assertions.
	os.Clearenv()
	os.Setenv("ANIMAL", "kitten")
	os.Setenv("INHERITED", "wazero")

	// We can't rely on the mtime from git because in CI, only the last commit
	// is checked out. Instead, grab the effective mtime.
	bearDir := filepath.Join(oldwd, "testdata", "fs")
	bearPath := filepath.Join(bearDir, "bear.txt")
	bearStat, err := os.Stat(bearPath)
	require.NoError(t, err)
	bearMtimeNano := bearStat.ModTime().UnixNano()

	existingDir1 := filepath.Join(tmpDir, "existing1")
	require.NoError(t, os.Mkdir(existingDir1, 0o700))
	existingDir2 := filepath.Join(tmpDir, "existing2")
	require.NoError(t, os.Mkdir(existingDir2, 0o700))

	cpuProfile := filepath.Join(t.TempDir(), "cpu.out")
	memProfile := filepath.Join(t.TempDir(), "mem.out")

	type test struct {
		name             string
		wazeroOpts       []string
		workdir          string
		wasm             []byte
		wasmArgs         []string
		expectedStdout   string
		expectedStderr   string
		expectedExitCode int
		test             func(t *testing.T)
	}

	tests := []test{
		{
			name:     "args",
			wasm:     wasmWasiArg,
			wasmArgs: []string{"hello world"},
			// Executable name is first arg so is printed.
			expectedStdout: "test.wasm\x00hello world\x00",
		},
		{
			name:     "-- args",
			wasm:     wasmWasiArg,
			wasmArgs: []string{"--", "hello world"},
			// Executable name is first arg so is printed.
			expectedStdout: "test.wasm\x00hello world\x00",
		},
		{
			name:           "env",
			wasm:           wasmWasiEnv,
			wazeroOpts:     []string{"--env=ANIMAL=bear", "--env=FOOD=sushi"},
			expectedStdout: "ANIMAL=bear\x00FOOD=sushi\x00",
		},
		{
			name:           "env-inherit",
			wasm:           wasmWasiEnv,
			wazeroOpts:     []string{"-env-inherit"},
			expectedStdout: "ANIMAL=kitten\x00INHERITED=wazero\u0000",
		},
		{
			name:           "env-inherit with env",
			wasm:           wasmWasiEnv,
			wazeroOpts:     []string{"-env-inherit", "--env=ANIMAL=bear"},
			expectedStdout: "ANIMAL=bear\x00INHERITED=wazero\u0000", // not ANIMAL=kitten
		},
		{
			name:           "interpreter",
			wasm:           wasmWasiArg,
			wazeroOpts:     []string{"--interpreter"}, // just test it works
			expectedStdout: "test.wasm\x00",
		},
		{
			name:           "wasi",
			wasm:           wasmWasiFd,
			wazeroOpts:     []string{fmt.Sprintf("--mount=%s:/", bearDir)},
			expectedStdout: "pooh\n",
		},
		{
			name:           "wasi readonly",
			wasm:           wasmWasiFd,
			wazeroOpts:     []string{fmt.Sprintf("--mount=%s:/:ro", bearDir)},
			expectedStdout: "pooh\n",
		},
		{
			name:           "wasi non root",
			wasm:           wasmCatTinygo,
			wazeroOpts:     []string{fmt.Sprintf("--mount=%s:/animals:ro", bearDir)},
			wasmArgs:       []string{"/animals/bear.txt"},
			expectedStdout: "pooh\n",
		},
		{
			name:       "wasi hostlogging=all",
			wasm:       wasmWasiRandomGet,
			wazeroOpts: []string{"--hostlogging=all"},
			expectedStderr: `--> .$1()
	==> wasi_snapshot_preview1.random_get(buf=0,buf_len=1000)
	<== errno=ESUCCESS
<--
`,
		},
		{
			name:       "wasi hostlogging=proc",
			wasm:       wasmCatTinygo,
			wazeroOpts: []string{"--hostlogging=proc", fmt.Sprintf("--mount=%s:/animals:ro", bearDir)},
			wasmArgs:   []string{"/animals/not-bear.txt"},
			expectedStderr: `==> wasi_snapshot_preview1.proc_exit(rval=1)
`, // ^^ proc_exit panics, which short-circuits the logger. Hence, no "<==".
			expectedExitCode: 1,
		},
		{
			name:       "wasi hostlogging=filesystem",
			wasm:       wasmCatTinygo,
			wazeroOpts: []string{"--hostlogging=filesystem", fmt.Sprintf("--mount=%s:/animals:ro", bearDir)},
			wasmArgs:   []string{"/animals/bear.txt"},
			expectedStderr: fmt.Sprintf(`==> wasi_snapshot_preview1.fd_prestat_get(fd=3)
<== (prestat={pr_name_len=8},errno=ESUCCESS)
==> wasi_snapshot_preview1.fd_prestat_dir_name(fd=3)
<== (path=/animals,errno=ESUCCESS)
==> wasi_snapshot_preview1.fd_prestat_get(fd=4)
<== (prestat=,errno=EBADF)
==> wasi_snapshot_preview1.fd_fdstat_get(fd=3)
<== (stat={filetype=DIRECTORY,fdflags=,fs_rights_base=FD_DATASYNC|FDSTAT_SET_FLAGS|FD_SYNC|PATH_CREATE_DIRECTORY|PATH_CREATE_FILE|PATH_LINK_SOURCE|PATH_LINK_TARGET|PATH_OPEN|FD_READDIR|PATH_READLINK,fs_rights_inheriting=FD_DATASYNC|FD_READ|FD_SEEK|FDSTAT_SET_FLAGS|FD_SYNC|FD_TELL|FD_WRITE|FD_ADVISE|FD_ALLOCATE|PATH_CREATE_DIRECTORY|PATH_CREATE_FILE|PATH_LINK_SOURCE|PATH_LINK_TARGET|PATH_OPEN|FD_READDIR|PATH_READLINK},errno=ESUCCESS)
==> wasi_snapshot_preview1.path_open(fd=3,dirflags=SYMLINK_FOLLOW,path=bear.txt,oflags=,fs_rights_base=FD_READ|FD_SEEK|FDSTAT_SET_FLAGS|FD_SYNC|FD_TELL|FD_ADVISE|PATH_CREATE_DIRECTORY|PATH_CREATE_FILE|PATH_LINK_SOURCE|PATH_LINK_TARGET|PATH_OPEN|FD_READDIR|PATH_READLINK|PATH_RENAME_SOURCE|PATH_RENAME_TARGET|PATH_FILESTAT_GET|PATH_FILESTAT_SET_SIZE|PATH_FILESTAT_SET_TIMES|FD_FILESTAT_GET|FD_FILESTAT_SET_TIMES|PATH_SYMLINK|PATH_REMOVE_DIRECTORY|PATH_UNLINK_FILE|POLL_FD_READWRITE,fs_rights_inheriting=FD_DATASYNC|FD_READ|FD_SEEK|FDSTAT_SET_FLAGS|FD_SYNC|FD_TELL|FD_WRITE|FD_ADVISE|FD_ALLOCATE|PATH_CREATE_DIRECTORY|PATH_CREATE_FILE|PATH_LINK_SOURCE|PATH_LINK_TARGET|PATH_OPEN|FD_READDIR|PATH_READLINK|PATH_RENAME_SOURCE|PATH_RENAME_TARGET|PATH_FILESTAT_GET|PATH_FILESTAT_SET_SIZE|PATH_FILESTAT_SET_TIMES|FD_FILESTAT_GET|FD_FILESTAT_SET_SIZE|FD_FILESTAT_SET_TIMES|PATH_SYMLINK|PATH_REMOVE_DIRECTORY|PATH_UNLINK_FILE|POLL_FD_READWRITE,fdflags=)
<== (opened_fd=4,errno=ESUCCESS)
==> wasi_snapshot_preview1.fd_filestat_get(fd=4)
<== (filestat={filetype=REGULAR_FILE,size=5,mtim=%d},errno=ESUCCESS)
==> wasi_snapshot_preview1.fd_read(fd=4,iovs=64664,iovs_len=1)
<== (nread=5,errno=ESUCCESS)
==> wasi_snapshot_preview1.fd_read(fd=4,iovs=64664,iovs_len=1)
<== (nread=0,errno=ESUCCESS)
==> wasi_snapshot_preview1.fd_close(fd=4)
<== errno=ESUCCESS
`, bearMtimeNano),
			expectedStdout: "pooh\n",
		},
		{
			name:       "wasi hostlogging=random",
			wasm:       wasmWasiRandomGet,
			wazeroOpts: []string{"--hostlogging=random"},
			expectedStderr: `==> wasi_snapshot_preview1.random_get(buf=0,buf_len=1000)
<== errno=ESUCCESS
`,
		},
		{
			name:       "cachedir existing absolute",
			wazeroOpts: []string{"--cachedir=" + existingDir1},
			wasm:       wasmWasiArg,
			wasmArgs:   []string{"hello world"},
			// Executable name is first arg so is printed.
			expectedStdout: "test.wasm\x00hello world\x00",
			test: func(t *testing.T) {
				entries, err := os.ReadDir(existingDir1)
				require.NoError(t, err)
				require.True(t, len(entries) > 0)
			},
		},
		{
			name:       "cachedir existing relative",
			wazeroOpts: []string{"--cachedir=existing2"},
			wasm:       wasmWasiArg,
			wasmArgs:   []string{"hello world"},
			// Executable name is first arg so is printed.
			expectedStdout: "test.wasm\x00hello world\x00",
			test: func(t *testing.T) {
				entries, err := os.ReadDir(existingDir2)
				require.NoError(t, err)
				require.True(t, len(entries) > 0)
			},
		},
		{
			name:       "cachedir new absolute",
			wazeroOpts: []string{"--cachedir=" + path.Join(tmpDir, "new1")},
			wasm:       wasmWasiArg,
			wasmArgs:   []string{"hello world"},
			// Executable name is first arg so is printed.
			expectedStdout: "test.wasm\x00hello world\x00",
			test: func(t *testing.T) {
				entries, err := os.ReadDir("new1")
				require.NoError(t, err)
				require.True(t, len(entries) > 0)
			},
		},
		{
			name:       "cachedir new relative",
			wazeroOpts: []string{"--cachedir=new2"},
			wasm:       wasmWasiArg,
			wasmArgs:   []string{"hello world"},
			// Executable name is first arg so is printed.
			expectedStdout: "test.wasm\x00hello world\x00",
			test: func(t *testing.T) {
				entries, err := os.ReadDir("new2")
				require.NoError(t, err)
				require.True(t, len(entries) > 0)
			},
		},
		{
			name:             "timeout: a binary that exceeds the deadline should print an error",
			wazeroOpts:       []string{"-timeout=1ms"},
			wasm:             wasmInfiniteLoop,
			expectedStderr:   "error: module closed with context deadline exceeded (timeout 1ms)\n",
			expectedExitCode: int(sys.ExitCodeDeadlineExceeded),
			test: func(t *testing.T) {
				require.NoError(t, err)
			},
		},
		{
			name:       "timeout: a binary that ends before the deadline should not print a timeout error",
			wazeroOpts: []string{"-timeout=10s"},
			wasm:       wasmWasiRandomGet,
			test: func(t *testing.T) {
				require.NoError(t, err)
			},
		},
		{
			name:             "should run wasi_unstable",
			wasm:             wasmWasiUnstable,
			expectedExitCode: 2,
			test: func(t *testing.T) {
				require.NoError(t, err)
			},
		},
		{
			name:       "enable cpu profiling",
			wazeroOpts: []string{"-cpuprofile=" + cpuProfile},
			wasm:       wasmWasiRandomGet,
			test: func(t *testing.T) {
				require.NoError(t, exist(cpuProfile))
			},
		},
		{
			name:       "enable memory profiling",
			wazeroOpts: []string{"-memprofile=" + memProfile},
			wasm:       wasmWasiRandomGet,
			test: func(t *testing.T) {
				require.NoError(t, exist(memProfile))
			},
		},
	}

	for _, tt := range tests {
		tc := tt

		if tc.wasm == nil {
			// We should only skip when the runtime is a scratch image.
			require.False(t, platform.CompilerSupported())
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			wasmPath := filepath.Join(tmpDir, "test.wasm")
			require.NoError(t, os.WriteFile(wasmPath, tc.wasm, 0o700))

			args := append([]string{"run"}, tc.wazeroOpts...)
			args = append(args, wasmPath)
			args = append(args, tc.wasmArgs...)
			exitCode, stdout, stderr := runMain(t, tc.workdir, args)

			require.Equal(t, tc.expectedStderr, stderr)
			require.Equal(t, tc.expectedExitCode, exitCode, stderr)
			require.Equal(t, tc.expectedStdout, stdout)
			if test := tc.test; test != nil {
				test(t)
			}
		})
	}
}

func TestVersion(t *testing.T) {
	exitCode, stdout, stderr := runMain(t, "", []string{"version"})
	require.Equal(t, 0, exitCode)
	require.Equal(t, version.GetWazeroVersion()+"\n", stdout)
	require.Equal(t, "", stderr)
}

func TestRun_Errors(t *testing.T) {
	wasmPath := filepath.Join(t.TempDir(), "test.wasm")
	require.NoError(t, os.WriteFile(wasmPath, wasmWasiArg, 0o700))

	notWasmPath := filepath.Join(t.TempDir(), "bears.wasm")
	require.NoError(t, os.WriteFile(notWasmPath, []byte("pooh"), 0o700))

	tests := []struct {
		message string
		args    []string
	}{
		{
			message: "missing path to wasm file",
			args:    []string{},
		},
		{
			message: "error reading wasm binary",
			args:    []string{"non-existent.wasm"},
		},
		{
			message: "error compiling wasm binary",
			args:    []string{notWasmPath},
		},
		{
			message: "invalid environment variable",
			args:    []string{"--env=ANIMAL", "testdata/wasi_env.wasm"},
		},
		{
			message: "invalid mount", // not found
			args:    []string{"--mount=te", "testdata/wasi_env.wasm"},
		},
		{
			message: "invalid cachedir",
			args:    []string{"--cachedir", notWasmPath, wasmPath},
		},
		{
			message: "timeout duration may not be negative",
			args:    []string{"-timeout=-10s", wasmPath},
		},
	}

	for _, tc := range tests {
		tt := tc
		t.Run(tt.message, func(t *testing.T) {
			exitCode, _, stderr := runMain(t, "", append([]string{"run"}, tt.args...))

			require.Equal(t, 1, exitCode)
			require.Contains(t, stderr, tt.message)
		})
	}
}

var _ api.FunctionDefinition = importer{}

type importer struct {
	internalapi.WazeroOnlyType
	moduleName, name string
}

func (i importer) ModuleName() string { return "" }
func (i importer) Index() uint32      { return 0 }
func (i importer) Import() (moduleName, name string, isImport bool) {
	return i.moduleName, i.name, true
}
func (i importer) ExportNames() []string        { return nil }
func (i importer) Name() string                 { return "" }
func (i importer) DebugName() string            { return "" }
func (i importer) GoFunction() interface{}      { return nil }
func (i importer) ParamTypes() []api.ValueType  { return nil }
func (i importer) ParamNames() []string         { return nil }
func (i importer) ResultTypes() []api.ValueType { return nil }
func (i importer) ResultNames() []string        { return nil }

func Test_detectImports(t *testing.T) {
	tests := []struct {
		message string
		imports []api.FunctionDefinition
		mode    importMode
	}{
		{
			message: "no imports",
		},
		{
			message: "other imports",
			imports: []api.FunctionDefinition{
				importer{internalapi.WazeroOnlyType{}, "env", "emscripten_notify_memory_growth"},
			},
		},
		{
			message: "wasi",
			imports: []api.FunctionDefinition{
				importer{internalapi.WazeroOnlyType{}, wasi_snapshot_preview1.ModuleName, "fd_read"},
			},
			mode: modeWasi,
		},
		{
			message: "unstable_wasi",
			imports: []api.FunctionDefinition{
				importer{internalapi.WazeroOnlyType{}, "wasi_unstable", "fd_read"},
			},
			mode: modeWasiUnstable,
		},
	}

	for _, tc := range tests {
		tt := tc
		t.Run(tt.message, func(t *testing.T) {
			mode := detectImports(tc.imports)
			require.Equal(t, tc.mode, mode)
		})
	}
}

func Test_logScopesFlag(t *testing.T) {
	tests := []struct {
		name     string
		values   []string
		expected logging.LogScopes
	}{
		{
			name:     "defaults to none",
			expected: logging.LogScopeNone,
		},
		{
			name:     "ignores empty",
			values:   []string{""},
			expected: logging.LogScopeNone,
		},
		{
			name:     "all",
			values:   []string{"all"},
			expected: logging.LogScopeAll,
		},
		{
			name:     "clock",
			values:   []string{"clock"},
			expected: logging.LogScopeClock,
		},
		{
			name:     "filesystem",
			values:   []string{"filesystem"},
			expected: logging.LogScopeFilesystem,
		},
		{
			name:     "memory",
			values:   []string{"memory"},
			expected: logging.LogScopeMemory,
		},
		{
			name:     "poll",
			values:   []string{"poll"},
			expected: logging.LogScopePoll,
		},
		{
			name:     "random",
			values:   []string{"random"},
			expected: logging.LogScopeRandom,
		},
		{
			name:     "clock filesystem poll random",
			values:   []string{"clock", "filesystem", "poll", "random"},
			expected: logging.LogScopeClock | logging.LogScopeFilesystem | logging.LogScopePoll | logging.LogScopeRandom,
		},
		{
			name:     "clock,filesystem poll,random",
			values:   []string{"clock,filesystem", "poll,random"},
			expected: logging.LogScopeClock | logging.LogScopeFilesystem | logging.LogScopePoll | logging.LogScopeRandom,
		},
		{
			name:     "all random",
			values:   []string{"all", "random"},
			expected: logging.LogScopeAll,
		},
	}

	for _, tt := range tests {
		tc := tt
		t.Run(tc.name, func(t *testing.T) {
			f := logScopesFlag(0)
			for _, v := range tc.values {
				require.NoError(t, f.Set(v))
			}
			require.Equal(t, tc.expected, logging.LogScopes(f))
		})
	}
}

func TestHelp(t *testing.T) {
	exitCode, _, stderr := runMain(t, "", []string{"-h"})
	require.Equal(t, 0, exitCode)
	fmt.Println(stderr)
	require.Equal(t, `wazero CLI

Usage:
  wazero <command>

Commands:
  compile	Pre-compiles a WebAssembly binary
  run		Runs a WebAssembly binary
  version	Displays the version of wazero CLI

Recording:
  Pass `+"`--out-dir <path>`"+` to `+"`wazero run`"+` to produce a CTFS trace bundle.
  The CODETRACER_WASM_RECORDER_OUT_DIR environment variable provides a
  fallback when --out-dir is omitted; CODETRACER_WASM_RECORDER_DISABLED=1
  skips recording entirely.  Use `+"`ct print`"+` from codetracer-trace-format-nim
  to convert the bundle to a human-readable JSON.  The `+"`wazero`"+` binary name
  is the one documented exception to the codetracer-<lang>-recorder pattern.

  Pass `+"`--boundary-log <path>`"+` to materialise a trace from a browser WASM
  boundary recording by re-executing the ORIGINAL, uninstrumented module
  against it (WASM-Instrumentation-Layer.md §6).

`+snapshotHelpBlock+`
`, stderr)
}

// snapshotHelpBlock is the build-variant-dependent tail of the usage text.
// The two artifacts (`just build` and `just build-snapshots`) advertise
// different capabilities per `WASM-Replay-Snapshots-And-Slices.md` §9, so the
// expectation has to follow the build tag the test itself was compiled with.
// It is derived from `snapshotsAvailable`, the same constant the usage text
// branches on, so the two cannot drift apart silently.
var snapshotHelpBlock = func() string {
	if snapshotsAvailable {
		return `  This build derives replay snapshots (--snapshots) and can materialise a
  sub-range of a recording without re-executing everything before it
  (--seek-from / --seek-to, WASM-Replay-Snapshots-And-Slices.md).`
	}
	return `  This build replays and materialises every recording completely and
  correctly, including one whose ` + "`.ct`" + ` already carries snapshot namespaces,
  which it reads and ignores.  It does not derive snapshots or seek with
  them, so reaching a point in the middle costs a linear replay.`
}()

// TestNoFormatFlagInHelp asserts that the legacy `--format` flag has been
// removed from the `wazero run` help output.  The wasm recorder is
// CTFS-only per Recorder-CLI-Conventions.md §4; the convention also forbids
// advertising a `CODETRACER_FORMAT` environment variable (§5).
//
// Pre-2026-05-08 the recorder shipped a `--format ctfs|binary|json|go`
// flag; the convention compliance pass dropped it.  See
// AUDIT-CTFS-2026-05.md "Convention compliance follow-up — 2026-05-08".
func TestNoFormatFlagInHelp(t *testing.T) {
	for _, args := range [][]string{
		{"-h"},
		{"run", "-h"},
		{"compile", "-h"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			_, stdout, stderr := runMain(t, "", args)
			combined := stdout + stderr
			require.False(t, strings.Contains(combined, "--format"),
				"help (%v) must not advertise --format; got:\n%s", args, combined)
			require.False(t, strings.Contains(combined, "-format "),
				"help (%v) must not advertise -format; got:\n%s", args, combined)
			require.False(t, strings.Contains(combined, "CODETRACER_FORMAT"),
				"help (%v) must not advertise CODETRACER_FORMAT; got:\n%s", args, combined)
		})
	}
}

// TestHelpMentionsCtPrint asserts that `--help` (top-level and `run`) points
// the user at `ct print` from codetracer-trace-format-nim — the canonical
// human-readable conversion tool for CTFS bundles
// (Recorder-CLI-Conventions.md §4).
func TestHelpMentionsCtPrint(t *testing.T) {
	for _, args := range [][]string{
		{"-h"},
		{"run", "-h"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			_, _, stderr := runMain(t, "", args)
			require.True(t, strings.Contains(stderr, "ct print"),
				"help (%v) must mention `ct print`; got:\n%s", args, stderr)
		})
	}
}

// TestFormatFlagRejected asserts that passing the legacy `--format` flag is
// rejected by the flag parser.  The wasm recorder is CTFS-only; an explicit
// rejection prevents downstream tooling from silently re-introducing the
// pre-2026-05-08 flag.
//
// The `run` subcommand uses `flag.ExitOnError`, so an unknown flag exits
// the process via `os.Exit(2)` directly.  We therefore re-exec the test
// binary as a subprocess (using the test-helper-via-env trick) and assert
// on its exit code and stderr without taking down the parent test runner.
func TestFormatFlagRejected(t *testing.T) {
	if os.Getenv("WAZERO_TEST_FORMAT_FLAG_HELPER") == "1" {
		// We are the helper subprocess.  Invoke doMain directly with
		// the offending args; flag.ExitOnError will call os.Exit(2).
		flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
		exitCode := doMain(os.Stdout, os.Stderr)
		os.Exit(exitCode)
		return
	}

	tmpDir := t.TempDir()
	wasmPath := filepath.Join(tmpDir, "test.wasm")
	require.NoError(t, os.WriteFile(wasmPath, wasmWasiArg, 0o600))

	cmd := exec.Command(os.Args[0],
		"-test.run", "TestFormatFlagRejected",
		"--",
		"run", "--format", "json", wasmPath)
	cmd.Env = append(os.Environ(), "WAZERO_TEST_FORMAT_FLAG_HELPER=1")
	out, err := cmd.CombinedOutput()
	combined := string(out)

	// The helper subprocess must exit non-zero.
	require.Error(t, err, "--format json must be rejected; combined output:\n%s", combined)
	require.True(t,
		strings.Contains(combined, "flag provided but not defined") ||
			strings.Contains(combined, "-format") ||
			strings.Contains(combined, "--format"),
		"flag parser should reject --format; got combined output:\n%s", combined)
}

// TestEnvOutDirUsedWhenFlagOmitted asserts the
// `CODETRACER_WASM_RECORDER_OUT_DIR` env var is honoured as the fallback
// for `--out-dir`.  Convention: Recorder-CLI-Conventions.md §5.
func TestEnvOutDirUsedWhenFlagOmitted(t *testing.T) {
	tmpDir := t.TempDir()
	wasmPath := filepath.Join(tmpDir, "test.wasm")
	require.NoError(t, os.WriteFile(wasmPath, wasmWasiArg, 0o700))

	envOutDir := filepath.Join(tmpDir, "via-env")

	defer func(prev string, ok bool) {
		if ok {
			_ = os.Setenv("CODETRACER_WASM_RECORDER_OUT_DIR", prev)
		} else {
			_ = os.Unsetenv("CODETRACER_WASM_RECORDER_OUT_DIR")
		}
	}(os.LookupEnv("CODETRACER_WASM_RECORDER_OUT_DIR"))
	defer func(prev string, ok bool) {
		if ok {
			_ = os.Setenv("CODETRACER_WASM_RECORDER_DISABLED", prev)
		} else {
			_ = os.Unsetenv("CODETRACER_WASM_RECORDER_DISABLED")
		}
	}(os.LookupEnv("CODETRACER_WASM_RECORDER_DISABLED"))

	require.NoError(t, os.Setenv("CODETRACER_WASM_RECORDER_OUT_DIR", envOutDir))
	_ = os.Unsetenv("CODETRACER_WASM_RECORDER_DISABLED")

	exitCode, _, stderr := runMain(t, "", []string{"run", wasmPath})
	require.Equal(t, 0, exitCode,
		"recorder should succeed when CODETRACER_WASM_RECORDER_OUT_DIR is set; stderr:\n%s",
		stderr)

	// The CTFS writer produces a single `<program-basename>.ct` container
	// in the env-supplied output dir.  Its presence is sufficient evidence
	// that the env var was honoured.
	matches, _ := filepath.Glob(filepath.Join(envOutDir, "*.ct"))
	require.True(t, len(matches) > 0,
		"env-supplied out dir %s should contain a .ct container; got: %v",
		envOutDir, matches)
}

// TestEnvDisabledSkipsRecording asserts that
// `CODETRACER_WASM_RECORDER_DISABLED` short-circuits recording even when
// `--out-dir` is set.  Convention: Recorder-CLI-Conventions.md §5.
func TestEnvDisabledSkipsRecording(t *testing.T) {
	tmpDir := t.TempDir()
	wasmPath := filepath.Join(tmpDir, "test.wasm")
	require.NoError(t, os.WriteFile(wasmPath, wasmWasiArg, 0o700))

	outDir := filepath.Join(tmpDir, "should-stay-empty")

	defer func(prev string, ok bool) {
		if ok {
			_ = os.Setenv("CODETRACER_WASM_RECORDER_DISABLED", prev)
		} else {
			_ = os.Unsetenv("CODETRACER_WASM_RECORDER_DISABLED")
		}
	}(os.LookupEnv("CODETRACER_WASM_RECORDER_DISABLED"))
	defer func(prev string, ok bool) {
		if ok {
			_ = os.Setenv("CODETRACER_WASM_RECORDER_OUT_DIR", prev)
		} else {
			_ = os.Unsetenv("CODETRACER_WASM_RECORDER_OUT_DIR")
		}
	}(os.LookupEnv("CODETRACER_WASM_RECORDER_OUT_DIR"))

	require.NoError(t, os.Setenv("CODETRACER_WASM_RECORDER_DISABLED", "1"))
	_ = os.Unsetenv("CODETRACER_WASM_RECORDER_OUT_DIR")

	exitCode, _, stderr := runMain(t, "",
		[]string{"run", "--out-dir=" + outDir, wasmPath})
	require.Equal(t, 0, exitCode,
		"recorder should succeed in disabled mode; stderr:\n%s", stderr)

	// The disabled-mode contract is "no trace artefacts written".  The
	// dir may exist as the parent (callers might pre-create it) but it
	// must contain no `trace*.json` / `*.ct` outputs.
	if entries, err := os.ReadDir(outDir); err == nil {
		for _, e := range entries {
			name := e.Name()
			require.False(t, strings.HasPrefix(name, "trace") ||
				strings.HasSuffix(name, ".ct"),
				"disabled mode must not write trace artefacts; found %q", name)
		}
	}
}

// TestRecordedTraceViaCtPrintJson records a small Rust-compiled WASM
// program with full DWARF debug info, then pipes the produced trace
// bundle through `ct print --full --strip-paths` from
// codetracer-trace-format-nim and asserts on **exact decoded values**.
//
// The fixture is compiled during this test run from
// `test_code/rust_test.rs`, the canonical debug-built WASM program of
// the wasm recorder (see `rust_fixture_test.go`).  The program is:
//
//	fn add_3_and_4() -> i32 {
//	    let blq = "abcd";    // line 16
//	    let x = 3;           // line 18
//	    let y = 4;           // line 19
//	    let test_struct = Sample { ... };  // line 21
//	    return x + y;        // line 31
//	}
//	fn main() {
//	    let z = add_3_and_4(); // line 37
//	}
//
// The DWARF-driven interpreter loop in
// `internal/engine/interpreter/interpreter.go` emits Step / Call /
// Function / Variable events for every source line and let-binding
// covered by the user-Rust filter (see CLAUDE.md "DWARF stepping
// details").  The `wasi_arg.wasm` upstream WAT fixture is intentionally
// stripped of DWARF and is unsuitable for value-level assertions.
//
// This test mirrors the 2026-05-08 precedent established by the
// cairo / cardano / circom / flow / fuel / leo / miden / move /
// polkavm / ruby / solana / ton recorders' `--full` upgrades:
//
//   - Strict `value.kind` invariant (switch over the decoded `kind`
//     field — Int, String, Bool, Reference, Struct, etc. — so a
//     ValueRecord variant change surfaces loudly rather than as a
//     silent existence-only weakening).
//   - Exact (varname, value) pair assertions against the literal
//     values in `rust_test.rs`.
//   - Function table assertion (`add_3_and_4` and `main` present,
//     matched via `strings.HasSuffix` to stay platform-agnostic).
//   - Path table assertion (the source `.rs` file appears).
//   - Step / call counts.
//   - Call sequence: main → add_3_and_4 (one nested call, return 7).
//
// `t.Skipf` (loud, on stderr) is reserved for the single audit-approved
// situation: `ct-print` binary not present (the workspace sibling is
// missing — usually because the repo is built outside metacraft).  Every
// other failure path fails the test loudly.  In particular, the legacy
// "Follow-up A" Skipf — which gated on `tracewriter.GoWriter` writing the
// three-file JSON layout (`trace.json` + `trace_metadata.json` +
// `trace_paths.json`) instead of a `.ct` container — was retired when the
// CTFS writer was wired up via cgo in `tracewriter/ctfs_writer.go` (see
// AUDIT-CTFS-2026-05.md "Convention compliance follow-up — 2026-05-08").
func TestRecordedTraceViaCtPrintJson(t *testing.T) {
	ctPrint := ctPrintPath(t)
	if _, err := os.Stat(ctPrint); err != nil {
		t.Skipf("SKIP: ct-print not found at %s — only available within the "+
			"metacraft workspace where codetracer-trace-format-nim is a sibling.",
			ctPrint)
	}

	tmpDir := t.TempDir()
	wasmPath := filepath.Join(tmpDir, "rust_test.wasm")
	// The fixture is compiled from `test_code/rust_test.rs` during this
	// run (see `rust_fixture_test.go`) — it carries DWARF, which is
	// required for the interpreter to emit source-level Step / Call /
	// Variable events.  The upstream `wasi_arg.wasm` is hand-written WAT
	// with no DWARF and would produce an empty trace.
	require.NoError(t, os.WriteFile(wasmPath, rustTestFixture(t), 0o700))

	outDir := filepath.Join(tmpDir, "traces")

	exitCode, _, stderr := runMain(t, "",
		[]string{"run", "--out-dir=" + outDir, wasmPath})
	require.Equal(t, 0, exitCode,
		"recorder should succeed; stderr:\n%s", stderr)

	// The CTFS writer produces a single `<program-basename>.ct` container
	// under outDir.  No legacy three-file fallback is accepted — the wasm
	// recorder is CTFS-only per Recorder-CLI-Conventions.md §4.
	candidates, _ := filepath.Glob(filepath.Join(outDir, "*.ct"))
	require.True(t, len(candidates) > 0,
		"expected at least one .ct artefact under %s", outDir)

	// Invoke `ct-print --full --strip-paths`.  The convention-compliance
	// substring-anchor layer (`--json` mode) is gone — `--full` is now
	// the canonical assertion surface because it decodes CBOR variable
	// payloads back into structured JSON suitable for golden / exact
	// comparisons.  See codetracer-trace-format-nim/src/codetracer_ct_print.nim
	// for the schema.
	cmd := exec.Command(ctPrint, "--full", "--strip-paths", candidates[0])
	out, err := cmd.CombinedOutput()
	require.NoError(t, err,
		"ct-print --full should succeed on the recorded trace; stderr/output:\n%s",
		string(out))

	require.True(t, len(out) > 0, "ct-print --full produced empty output")

	// Decode the deterministic JSON document.  Top-level keys per the
	// `--full` schema: metadata, paths, functions, varnames, types,
	// counts, events.
	var doc struct {
		Metadata struct {
			Program string   `json:"program"`
			Args    []string `json:"args"`
			Workdir string   `json:"workdir"`
		} `json:"metadata"`
		Paths     []string          `json:"paths"`
		Functions []string          `json:"functions"`
		Varnames  []string          `json:"varnames"`
		Counts    map[string]int    `json:"counts"`
		Events    []json.RawMessage `json:"events"`
	}
	require.NoError(t, json.Unmarshal(out, &doc),
		"ct-print --full should emit valid JSON; got:\n%s", string(out))

	// ----------------------------------------------------------------
	// Sanity: ct-print must have parsed the .ct container into a
	// non-empty document.  Sentinel `counts.steps == -1` or empty
	// events/functions arrays would indicate the FFI emitted a layout
	// ct-print cannot read — fail loudly so the regression surfaces.
	// ----------------------------------------------------------------
	require.False(t, doc.Counts["steps"] == -1,
		"ct-print returned sentinel counts (-1) for %s — the .ct container "+
			"could not be parsed; counts=%v", candidates[0], doc.Counts)
	require.True(t, len(doc.Events) > 0 || len(doc.Functions) > 0,
		"ct-print --full produced an empty document for %s (counts=%v)",
		candidates[0], doc.Counts)

	// ================================================================
	// Layer 2: exact decoded values via ct-print --full
	// ================================================================
	// All assertions below run only when the trace is genuinely
	// readable by ct-print (i.e., a true .ct container is produced).

	// ----- Function table: add_3_and_4 and main both present --------
	require.True(t, anyHasSuffix(doc.Functions, "add_3_and_4"),
		"expected `add_3_and_4` in functions table; got %v", doc.Functions)
	require.True(t, anyHasSuffix(doc.Functions, "main"),
		"expected `main` in functions table; got %v", doc.Functions)

	// ----- Path table: rust_test.rs source path appears -------------
	require.True(t, anyHasSuffix(doc.Paths, "rust_test.rs"),
		"expected rust_test.rs in paths table; got %v", doc.Paths)

	// ----- Step / call counts ---------------------------------------
	// `add_3_and_4` covers source lines 16/18/19/21/31 (5 lines) plus
	// the `main` entry-line step on 37 — six step events at minimum.
	// We assert lower bounds rather than exact counts because future
	// inlined-subroutine entries from libcore could legitimately add
	// synthetic steps; a regression that drops a whole stream still
	// fails the lower bound.
	require.True(t, doc.Counts["steps"] >= 6,
		"expected at least 6 step events for rust_test.rs; counts=%v",
		doc.Counts)
	require.True(t, doc.Counts["calls"] >= 2,
		"expected at least 2 call events (main + add_3_and_4); counts=%v",
		doc.Counts)

	// ----- Call sequence: main → add_3_and_4 ------------------------
	// We collect every `call_entry` event's function name and assert
	// both expected callees appear in order.  HasSuffix keeps the
	// match platform-agnostic if a future writer ever qualifies names
	// with module paths (e.g. `rust_test::add_3_and_4`).
	var callSequence []string
	type evtHeader struct {
		Kind        string          `json:"kind"`
		Function    string          `json:"function"`
		ReturnValue json.RawMessage `json:"return_value"`
		Vars        json.RawMessage `json:"vars"`
	}
	var typedEvents []evtHeader
	for _, raw := range doc.Events {
		var hdr evtHeader
		require.NoError(t, json.Unmarshal(raw, &hdr),
			"every --full event must decode against the documented schema; "+
				"got:\n%s", string(raw))
		typedEvents = append(typedEvents, hdr)
		if hdr.Kind == "call_entry" {
			callSequence = append(callSequence, hdr.Function)
		}
	}
	require.True(t, indexHasSuffix(callSequence, "main") <
		indexHasSuffix(callSequence, "add_3_and_4"),
		"expected call sequence main → add_3_and_4; got %v", callSequence)

	// ----- Strict value.kind invariant + exact (varname, value) -----
	// Decode every variable surfaced by step events.  Each entry must
	// declare a `kind` field; we switch over the canonical CBOR
	// ValueRecord variants.  Anything new fails loudly so a recorder
	// upgrade adding (say) a tagged variant for Rust string slices
	// can't silently bypass the assertion layer.
	type varEntry struct {
		Varname string          `json:"varname"`
		Value   json.RawMessage `json:"value"`
	}
	type valueHeader struct {
		Kind   string `json:"kind"`
		I      *int64 `json:"i,omitempty"`
		Text   string `json:"text,omitempty"`
		B      *bool  `json:"b,omitempty"`
		TypeID *int64 `json:"type_id,omitempty"`
	}

	var observed []observedPair

	for _, hdr := range typedEvents {
		if hdr.Kind != "step" || len(hdr.Vars) == 0 {
			continue
		}
		var vars []varEntry
		require.NoError(t, json.Unmarshal(hdr.Vars, &vars),
			"step.vars must be an array of {varname,value}; got:\n%s",
			string(hdr.Vars))
		for _, v := range vars {
			var vh valueHeader
			require.NoError(t, json.Unmarshal(v.Value, &vh),
				"variable `%s` value must declare a `kind` field; got:\n%s",
				v.Varname, string(v.Value))
			pair := observedPair{Name: v.Varname, Kind: vh.Kind}
			switch vh.Kind {
			case "Int":
				require.NotNil(t, vh.I,
					"variable `%s` declared kind=Int but has no `i` field; got:\n%s",
					v.Varname, string(v.Value))
				pair.I = *vh.I
			case "String":
				pair.Text = vh.Text
			case "Bool":
				require.NotNil(t, vh.B,
					"variable `%s` declared kind=Bool but has no `b` field; got:\n%s",
					v.Varname, string(v.Value))
				pair.B = *vh.B
			case "Reference", "Struct", "None":
				// Compound / sentinel kinds — no scalar payload to
				// extract for this test, but the kind is recorded so
				// the structural assertions below still run.
			default:
				t.Fatalf("variable `%s` decoded as unrecognised kind=%q; if a "+
					"new ValueRecord variant has landed in the wasm recorder, "+
					"extend this test to assert on it explicitly rather than "+
					"silently widening the switch.  raw value:\n%s",
					v.Varname, vh.Kind, string(v.Value))
			}
			observed = append(observed, pair)
		}
	}

	// Exact (varname, value) pairs from `test_code/rust_test.rs`:
	//
	//   line 16  let blq = "abcd";   →  String "abcd"
	//   line 18  let x   = 3;        →  Int 3
	//   line 19  let y   = 4;        →  Int 4
	//
	// (`test_struct` is a Sample {…} value — recorded as a Struct
	// ValueRecord; covered by the strict-kind switch above and asserted
	// for presence here.)
	requireIntPair(t, observed, "x", 3)
	requireIntPair(t, observed, "y", 4)
	requireStringPair(t, observed, "blq", "abcd")
	require.True(t, anyKind(observed, "test_struct", "Struct"),
		"expected `test_struct` to surface as a Struct ValueRecord; "+
			"observed=%v", observed)

	// ----- Call exit return value: add_3_and_4() returns 7 ----------
	// `return x + y` with x=3, y=4 — the CBOR return-value blob must
	// decode as Int{i:7}.  If the recorder ever stops emitting return
	// values or starts emitting them with a different ValueRecord
	// variant we want to know loudly.
	var returnValues []json.RawMessage
	for _, hdr := range typedEvents {
		if hdr.Kind == "call_exit" && strings.HasSuffix(hdr.Function, "add_3_and_4") {
			returnValues = append(returnValues, hdr.ReturnValue)
		}
	}
	require.True(t, len(returnValues) >= 1,
		"expected at least one call_exit for `add_3_and_4`; got %d",
		len(returnValues))
	var rvHeader valueHeader
	require.NoError(t, json.Unmarshal(returnValues[0], &rvHeader),
		"call_exit.return_value must decode against the documented schema; "+
			"got:\n%s", string(returnValues[0]))
	require.Equal(t, "Int", rvHeader.Kind,
		"add_3_and_4() return_value should decode as Int; got %s",
		string(returnValues[0]))
	require.NotNil(t, rvHeader.I,
		"add_3_and_4() return_value Int payload missing `i` field; got %s",
		string(returnValues[0]))
	require.Equal(t, int64(7), *rvHeader.I,
		"add_3_and_4() should return 7 (3+4); got %s",
		string(returnValues[0]))
}

// anyHasSuffix reports whether any element of xs ends with sfx.  Used
// to keep function/path table assertions platform-agnostic in case a
// future writer qualifies names with module paths.
func anyHasSuffix(xs []string, sfx string) bool {
	for _, x := range xs {
		if strings.HasSuffix(x, sfx) {
			return true
		}
	}
	return false
}

// indexHasSuffix returns the first index i such that xs[i] ends with
// sfx, or len(xs) if no such i exists.  Used to assert call-sequence
// ordering (caller appears before callee).
func indexHasSuffix(xs []string, sfx string) int {
	for i, x := range xs {
		if strings.HasSuffix(x, sfx) {
			return i
		}
	}
	return len(xs)
}

// requireIntPair fails the test if no observed step variable matches
// (name, kind=Int, i=value).  The error message lists every observed
// pair so a drift in the recorder is immediately visible.
func requireIntPair(t *testing.T, obs []observedPair, name string, value int64) {
	t.Helper()
	for _, p := range obs {
		if p.Name == name && p.Kind == "Int" && p.I == value {
			return
		}
	}
	t.Fatalf("expected step variable `%s` = %d (kind=Int) in --full output; "+
		"observed = %v", name, value, obs)
}

// requireStringPair fails the test if no observed step variable
// matches (name, kind=String, text=value).
func requireStringPair(t *testing.T, obs []observedPair, name string, value string) {
	t.Helper()
	for _, p := range obs {
		if p.Name == name && p.Kind == "String" && p.Text == value {
			return
		}
	}
	t.Fatalf("expected step variable `%s` = %q (kind=String) in --full output; "+
		"observed = %v", name, value, obs)
}

// anyKind reports whether any observed step variable matches
// (name, kind).  Used for compound types (Struct, Reference) where
// the scalar payload isn't asserted but the kind itself is.
func anyKind(obs []observedPair, name string, kind string) bool {
	for _, p := range obs {
		if p.Name == name && p.Kind == kind {
			return true
		}
	}
	return false
}

// observedPair carries (name, kind, scalar payload) for every variable
// surfaced by a step event.  Defined at package scope so the helpers
// above can reference it without a closure.
type observedPair struct {
	Name string
	Kind string
	I    int64
	Text string
	B    bool
}

// ctPrintPath returns the location of the `ct-print` binary shipped with
// codetracer-trace-format-nim within the metacraft workspace.  The wasm
// recorder lives at metacraft/codetracer-wasm-recorder/ so the sibling
// repo is at metacraft/codetracer-trace-format-nim/.
func ctPrintPath(t *testing.T) string {
	t.Helper()
	// Repo root is two levels up from cmd/wazero/.
	wd, err := os.Getwd()
	require.NoError(t, err)
	root := filepath.Dir(filepath.Dir(wd))
	return filepath.Join(root, "..", "codetracer-trace-format-nim", "ct-print")
}

func runMain(t *testing.T, workdir string, args []string) (int, string, string) {
	t.Helper()

	// Use a workdir override if supplied.
	if workdir != "" {
		oldcwd, err := os.Getwd()
		require.NoError(t, err)

		require.NoError(t, os.Chdir(workdir))
		defer func() {
			require.NoError(t, os.Chdir(oldcwd))
		}()
	}

	oldArgs := os.Args
	t.Cleanup(func() {
		os.Args = oldArgs
	})
	os.Args = append([]string{"wazero"}, args...)
	flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ContinueOnError)

	stdout := new(bytes.Buffer)
	stderr := new(bytes.Buffer)
	exitCode := doMain(stdout, stderr)

	return exitCode, stdout.String(), stderr.String()
}

func exist(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	return f.Close()
}
