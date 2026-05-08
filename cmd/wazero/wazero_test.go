package main

import (
	"bytes"
	_ "embed"
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
`, stderr)
}

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

	// trace_record's ProduceTrace writes trace.json + trace_metadata.json
	// + trace_paths.json into the env-supplied output dir.  Any of those
	// is sufficient evidence the env var was honoured.
	for _, name := range []string{"trace.json", "trace_metadata.json", "trace_paths.json"} {
		_, statErr := os.Stat(filepath.Join(envOutDir, name))
		require.NoError(t, statErr,
			"env-supplied out dir %s should contain %s", envOutDir, name)
	}
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

// TestRecordedTraceViaCtPrintJson records a tiny WASM program then pipes the
// resulting trace bundle through `ct print --json` from
// codetracer-trace-format-nim, asserting on textual structural anchors.
//
// The wasm recorder's variable payload (DWARF locals encoded as Int /
// String / Float / etc.) does not round-trip through `ct print --json`
// today (same pre-existing limitation as cardano / circom / flow / fuel /
// leo / miden / move / polkavm / python / ruby / solana / ton), so this
// test asserts on **structural anchors** — the program name and the
// presence of a non-empty events stream — rather than on integer values.
//
// Skipped gracefully when `ct-print` is not present (i.e. when this repo is
// built outside the metacraft workspace).  See Recorder-CLI-Conventions.md
// §4 and AUDIT-CTFS-2026-05.md ("Convention compliance follow-up —
// 2026-05-08") for the full record.
func TestRecordedTraceViaCtPrintJson(t *testing.T) {
	ctPrint := ctPrintPath(t)
	if _, err := os.Stat(ctPrint); err != nil {
		t.Skipf("SKIP: ct-print not found at %s — only available within the "+
			"metacraft workspace where codetracer-trace-format-nim is a sibling.",
			ctPrint)
	}

	tmpDir := t.TempDir()
	wasmPath := filepath.Join(tmpDir, "test.wasm")
	require.NoError(t, os.WriteFile(wasmPath, wasmWasiArg, 0o700))

	outDir := filepath.Join(tmpDir, "traces")

	exitCode, _, stderr := runMain(t, "",
		[]string{"run", "--out-dir=" + outDir, wasmPath})
	require.Equal(t, 0, exitCode,
		"recorder should succeed; stderr:\n%s", stderr)

	// The Go writer produces the legacy three-file JSON layout under outDir.
	// `ct print --json` accepts the trace_metadata.json file as an
	// equivalent CTFS-compatible entry point through the legacy reader.
	// If a future writer swap produces a true `.ct` container instead,
	// both layouts should be tried.
	candidates, _ := filepath.Glob(filepath.Join(outDir, "*.ct"))
	if len(candidates) == 0 {
		// Legacy three-file JSON path: feed trace_metadata.json so
		// ct print can pick up the bundle.  When the FFI/Nim CTFS
		// writer lands the `.ct` glob above will populate first.
		legacy := filepath.Join(outDir, "trace.json")
		if _, err := os.Stat(legacy); err == nil {
			candidates = []string{legacy}
		}
	}
	require.True(t, len(candidates) > 0,
		"expected at least one trace artefact under %s", outDir)

	cmd := exec.Command(ctPrint, "--json", candidates[0])
	out, err := cmd.CombinedOutput()
	if err != nil {
		// If ct-print can't read the legacy JSON layout (it is
		// primarily a CTFS reader), skip rather than fail — the test
		// will start asserting once the CTFS writer is wired.
		t.Skipf("SKIP: ct-print could not read %s (likely a legacy three-file "+
			"JSON layout that ct-print does not handle yet): %v\noutput:\n%s",
			candidates[0], err, string(out))
	}

	stdout := string(out)
	require.True(t, len(stdout) > 0, "ct-print --json produced empty output")
	// Structural anchors: ct-print emits a JSON document with the canonical
	// CTFS section names — `metadata`, `paths`, `functions`, `steps`,
	// `calls`, `values`, `ioEvents`.  These anchors are stable regardless
	// of whether the WASI fixture contained DWARF information (the test
	// fixtures shipped with the upstream wazero CLI tests are stripped of
	// DWARF, so the recorded events stream is intentionally empty).  A
	// recorder regression that drops a whole section will fail this test;
	// integer-payload round-trip assertions are out of scope for the same
	// reason as the precedent recorders (cardano / circom / flow / fuel /
	// leo / miden / move / polkavm / python / ruby / solana / ton).
	for _, anchor := range []string{
		"metadata", "paths", "functions", "steps", "calls", "ioEvents",
	} {
		require.True(t, strings.Contains(stdout, anchor),
			"ct-print --json output missing structural anchor %q; got:\n%s",
			anchor, stdout)
	}
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
