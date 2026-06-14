package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/warxhead1/claude-hook-fast/tools/claude-hooks/internal/hookio"
)

// cmdShaderCompileValidator is a PostToolUse BLOCKING hook.
//
// Fires on Write|Edit|MultiEdit for .slang/.glsl files under a /shaders/ path
// in the tengine project. Compiles the affected shader (or its entry-point
// parent, for module files) via `shadergen check-shader`. Exits 2 if the
// compilation fails so Claude must fix the error before continuing.
//
// Fast-path gates (all checked before any disk I/O):
//  1. Tool name must be Write, Edit, or MultiEdit.
//  2. File must be in tengine (isTengineFile).
//  3. File must be .slang or .glsl (isShaderFile).
//  4. File must live under a /shaders/ directory component.
//  5. File must not be generated (isGeneratedShader).
func cmdShaderCompileValidator() {
	input, ok := hookio.ReadHookInput()
	if !ok {
		return
	}
	ex := extractEditContent(input)
	if ex == nil || ex.filePath == "" {
		return
	}

	fp := ex.filePath

	// Gates: tengine shader, not generated.
	if !isTengineFile(fp) || !isShaderFile(fp) || isGeneratedShader(fp) {
		return
	}
	// Must live somewhere under a /shaders/ directory.
	if !strings.Contains(fp, "/shaders/") {
		return
	}

	// Determine what to compile.
	target, reason := resolveCompileTarget(fp)
	if target == "" {
		// Can't determine a compile target — skip silently.
		return
	}

	// Find shadergen.
	shadergenPath := findShadergen()
	if shadergenPath == "" {
		// No shadergen available — skip silently rather than false-positive.
		return
	}

	// Run check-shader with a 15-second timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, shadergenPath, "check-shader", target)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err == nil {
		// Compile succeeded.
		return
	}

	// Compile failed — block the agent.
	baseFile := filepath.Base(fp)
	targetBase := filepath.Base(target)
	var msg strings.Builder
	fmt.Fprintf(&msg, "SHADER COMPILE ERROR in %s", baseFile)
	if targetBase != baseFile {
		fmt.Fprintf(&msg, " (compiled via %s — %s)", targetBase, reason)
	}
	fmt.Fprintf(&msg, ":\n\n")
	diagOut := strings.TrimSpace(stderr.String())
	if diagOut != "" {
		fmt.Fprintln(&msg, diagOut)
	} else {
		fmt.Fprintln(&msg, err.Error())
	}

	// Reflexarc enrichment: turn raw "undeclared identifier"/"cannot find include"
	// diagnostics into the EXACT deterministic fix (which import/include to add).
	// Best-effort — never changes pass/fail, only augments the message.
	if fixes := reflexarcSlangFixes(fp, diagOut); fixes != "" {
		fmt.Fprintf(&msg, "\n🛰 Reflexarc — deterministic fix:\n%s\n", fixes)
	}

	fmt.Fprintln(&msg, "\nFix the compile error before continuing.")

	fmt.Fprint(os.Stderr, msg.String())
	os.Exit(2)
}

// reflexarcSlangFixes shells out to the reflex-tengine slang resolver, which maps
// undeclared RES_*/WT_* symbols → the exact `import <leaf_shard>;` and bad #include
// paths → the correct path. Returns "" on any failure (resolver absent, timeout,
// no deterministic fix) so this never disrupts the validator.
func reflexarcSlangFixes(fp, compilerStderr string) string {
	if compilerStderr == "" {
		return ""
	}
	resolver := os.Getenv("REFLEX_SLANG_RESOLVER")
	if resolver == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		resolver = filepath.Join(home, ".config", "nervous-bus", "adapters",
			"reflex-tengine", "sentinels", "slang_edit.py")
	}
	if _, err := os.Stat(resolver); err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "python3", resolver, "--resolve", fp)
	cmd.Stdin = strings.NewReader(compilerStderr)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return ""
	}
	return strings.TrimSpace(out.String())
}

// resolveCompileTarget returns the shader file to compile and a human-readable
// reason string. If the edited file has an entry point it is used directly. For
// module files (no entry point) we search for an importing entry-point shader.
// Returns ("", "") when no suitable target can be found.
func resolveCompileTarget(fp string) (target string, reason string) {
	data, err := os.ReadFile(fp)
	if err != nil {
		return "", ""
	}
	content := string(data)

	if hasEntryPoint(content) {
		return fp, "direct entry-point"
	}

	// Module file — find a parent entry-point shader that imports it.
	moduleName := moduleNameFromPath(fp)
	if moduleName == "" {
		return "", ""
	}

	parent := findImportingEntryPoint(fp, moduleName)
	if parent == "" {
		return "", ""
	}
	return parent, fmt.Sprintf("imports %s", moduleName)
}

// hasEntryPoint returns true if the content contains a Slang or GLSL entry
// point marker. Uses simple string search — no regex — for speed.
func hasEntryPoint(content string) bool {
	return strings.Contains(content, "[shader(") ||
		strings.Contains(content, "[numthreads(") ||
		strings.Contains(content, "void main(")
}

// moduleNameFromPath derives the Slang module name from the file path
// (filename without extension, as Slang resolves `import foo` to `foo.slang`).
func moduleNameFromPath(fp string) string {
	base := filepath.Base(fp)
	ext := filepath.Ext(base)
	if ext == "" {
		return ""
	}
	return strings.TrimSuffix(base, ext)
}

// findImportingEntryPoint searches for a shader file that (a) imports the
// given module and (b) has an entry point. It searches the same directory as
// fp, then its parent, to keep the walk bounded and fast.
func findImportingEntryPoint(fp, moduleName string) string {
	needle := "import " + moduleName

	dir := filepath.Dir(fp)
	parent := filepath.Dir(dir)

	// Search order: same dir, then parent dir. Both are shallow — no recursion.
	for _, searchDir := range []string{dir, parent} {
		if found := searchDirForImporter(searchDir, needle); found != "" {
			return found
		}
	}

	// Widen to the full shaders/ subtree only if the shallow search fails.
	shadersRoot := shadersRootFrom(fp)
	if shadersRoot == "" || shadersRoot == dir || shadersRoot == parent {
		return ""
	}
	return searchTreeForImporter(shadersRoot, needle)
}

// searchDirForImporter scans a single directory (non-recursive) for a .slang
// file that contains needle and has an entry point.
func searchDirForImporter(dir, needle string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".slang") && !strings.HasSuffix(name, ".glsl") {
			continue
		}
		candidate := filepath.Join(dir, name)
		data, err := os.ReadFile(candidate)
		if err != nil {
			continue
		}
		content := string(data)
		if strings.Contains(content, needle) && hasEntryPoint(content) {
			return candidate
		}
	}
	return ""
}

// searchTreeForImporter walks the shaders subtree looking for an importer with
// an entry point. Terminates on first match to keep it fast.
func searchTreeForImporter(root, needle string) string {
	var found string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || found != "" {
			return nil
		}
		if d.IsDir() {
			// Skip generated dirs early.
			name := d.Name()
			if name == "slang_generated" || name == "generated" {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".slang") && !strings.HasSuffix(name, ".glsl") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		content := string(data)
		if strings.Contains(content, needle) && hasEntryPoint(content) {
			found = path
		}
		return nil
	})
	return found
}

// shadersRootFrom walks up from the file's directory until it finds a
// directory named "shaders", or returns "".
func shadersRootFrom(fp string) string {
	dir := filepath.Dir(fp)
	for {
		if filepath.Base(dir) == "shaders" {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// findShadergen returns the path to the shadergen binary.
func findShadergen() string {
	// 1. PATH
	if p, err := exec.LookPath("shadergen"); err == nil {
		return p
	}
	home, err := os.UserHomeDir()
	if err == nil {
		// 2. Tengine release build
		if p := filepath.Join(home, "projects/tengine/target/release/shadergen"); fileExists(p) {
			return p
		}
		// 3. Tengine debug build
		if p := filepath.Join(home, "projects/tengine/target/debug/shadergen"); fileExists(p) {
			return p
		}
	}
	return ""
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
