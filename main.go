package main

import (
	"bytes"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"

	"github.com/BurntSushi/toml"
	"github.com/charmbracelet/lipgloss"
	apkg "github.com/innatical/apkg/v2/util"
	pax "github.com/innatical/pax/v3/util"
	"github.com/urfave/cli/v2"

	"github.com/innatical/pax-chroot/util"
	lua "github.com/yuin/gopher-lua"
)

var errorStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FF0000"))

func main() {
	app := &cli.App{
		Name:      "apkgbuild",
		Usage:     "APKG Build Tool",
		UsageText: "apkgbuild [options] <input> <output>",
		Action:    mainCommand,
	}

	if err := app.Run(os.Args); err != nil {
		println(errorStyle.Render("Error: ") + err.Error())
		os.Exit(1)
	}
}

func Exec(L *lua.LState) int {
	command := L.ToString(1)

	shell, ok := L.GetGlobal("shell").(lua.LString)
	if !ok {
		panic("shell must be set")
	}

	cmd := exec.Command(shell.String(), "-c", command)
	cmd.Stdout = os.Stdout
	cmd.Stdin = os.Stdin
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		L.Push(lua.LBool(false))
		L.Push(lua.LString(err.Error()))
		return 2
	}

	L.Push(lua.LBool(true))

	return 1
}

func Download(L *lua.LState) int {
	url := L.ToString(1)
	file := L.ToString(2)

	resp, err := http.Get(url)
	if err != nil {
		L.Push(lua.LBool(false))
		L.Push(lua.LString(err.Error()))
		return 2
	}

	defer resp.Body.Close()

	f, err := os.Create(file)
	if err != nil {
		L.Push(lua.LBool(false))
		L.Push(lua.LString(err.Error()))
		return 2
	}

	defer f.Close()

	if _, err := io.Copy(f, resp.Body); err != nil {
		L.Push(lua.LBool(false))
		L.Push(lua.LString(err.Error()))
		return 2
	}

	L.Push(lua.LBool(true))

	return 1
}

func mainCommand(c *cli.Context) error {
	name, err := ioutil.TempDir("/tmp", "pax-chroot")
	if err != nil {
		return err
	}

	defer os.RemoveAll(name)

	if err := util.SetupChroot(name); err != nil {
		return err
	}

	defer util.CleanupChroot(name)

	cwd, err := os.Getwd()
	if err != nil {
		return err
	}

	if err := util.BindMount(name, "/mnt", cwd); err != nil {
		return err
	}

	defer util.UnmountBind(name, "/mnt")

	err = util.Cp(filepath.Join(os.Getenv("HOME"), "/.apkg/repos.toml"), filepath.Join(name, "repos.toml"))
	if err != nil {
		return err
	}

	L := lua.NewState()
	defer L.Close()
	if err := L.DoFile(c.Args().Get(0)); err != nil {
		return err
	}

	L.SetGlobal("exec", L.NewFunction(Exec))
	L.SetGlobal("download", L.NewFunction(Download))

	buildDependencies, ok := L.GetGlobal("build_dependencies").(*lua.LTable)
	if !ok {
		return &apkg.ErrorString{"Could not parse build_dependencies"}
	}

	var deps []string

	buildDependencies.ForEach(func(l1, l2 lua.LValue) {
		deps = append(deps, l1.String()+"@"+l2.String())
	})

	println("Installing build dependencies...")

	usr, err := user.Current()
	if err != nil {
		println(err.Error())
		os.Exit(1)
	}

	if err := pax.InstallMultiple(name, filepath.Join(usr.HomeDir, "/.apkg", "cache"), deps, true); err != nil {
		return err
	}

	if err := os.Mkdir(filepath.Join(name, "/pkg"), 0777); err != nil {
		return err
	}

	curr, err := os.Getwd()

	if err != nil {
		return nil
	}

	println("Entering chroot and running build function...")

	if err := func() error {
		exit, err := util.OpenChroot(name)
		if err != nil {
			return err
		}

		defer exit()

		if err := L.CallByParam(lua.P{
			Fn:      L.GetGlobal("build"),
			NRet:    1,
			Protect: true,
		}); err != nil {
			exit()
			return err
		}

		return nil
	}(); err != nil {
		return err
	}

	pkgName, ok := L.GetGlobal("name").(lua.LString)
	if !ok {
		return &apkg.ErrorString{"Package name not defined"}
	}

	pkgVersion, ok := L.GetGlobal("version").(lua.LString)
	if !ok {
		return &apkg.ErrorString{"Package version not defined"}
	}

	pkgDescription, ok := L.GetGlobal("description").(lua.LString)
	if !ok {
		return &apkg.ErrorString{"Package description not defined"}
	}

	pkgAuthors, ok := L.GetGlobal("authors").(*lua.LTable)
	if !ok {
		return &apkg.ErrorString{"Package authors not defined"}
	}

	pkgAuthorsList := []string{}

	pkgAuthors.ForEach(func(l1, l2 lua.LValue) {
		pkgAuthorsList = append(pkgAuthorsList, l2.String())
	})

	pkgMaintainers, ok := L.GetGlobal("maintainers").(*lua.LTable)
	if !ok {
		return &apkg.ErrorString{"Package maintainers not defined"}
	}

	pkgMaintainersList := []string{}

	pkgMaintainers.ForEach(func(l1, l2 lua.LValue) {
		pkgMaintainersList = append(pkgMaintainersList, l2.String())
	})

	pkgDependencies, ok := L.GetGlobal("dependencies").(*lua.LTable)
	if !ok {
		return &apkg.ErrorString{"Package dependencies not defined"}
	}

	pkgRequiredDependenciesList := []string{}
	pkgOptionalDependenciesList := []string{}

	pkgRequiredDepedencies, ok := pkgDependencies.RawGetString("required").(*lua.LTable)

	if !ok {
		return &apkg.ErrorString{"Required dependencies not defined"}
	}

	pkgRequiredDepedencies.ForEach(func(l1, l2 lua.LValue) {
		pkgRequiredDependenciesList = append(pkgRequiredDependenciesList, l1.String()+"@"+l2.String())
	})

	pkgOptionalDependencies, ok := pkgDependencies.RawGetString("optional").(*lua.LTable)

	if !ok {
		return &apkg.ErrorString{"Optional dependencies not defined"}
	}

	pkgOptionalDependencies.ForEach(func(l1, l2 lua.LValue) {
		pkgOptionalDependenciesList = append(pkgOptionalDependenciesList, l1.String()+"@"+l2.String())
	})

	pkgFilesMap := make(map[string]string)

	pkgFiles, ok := L.GetGlobal("files").(*lua.LTable)

	if !ok {
		return &apkg.ErrorString{"Package files not defined"}
	}

	pkgFiles.ForEach(func(l1, l2 lua.LValue) {
		pkgFilesMap[l1.String()] = l2.String()
	})

	pkgHooks, ok := L.GetGlobal("hooks").(*lua.LTable)
	if !ok {
		return &apkg.ErrorString{"Package hooks not defined"}
	}

	pkgPreinstallString := ""
	pkgPreinstall, ok := pkgHooks.RawGetString("preinstall").(lua.LString)
	if ok {
		pkgPreinstallString = pkgPreinstall.String()
	}

	pkgPostinstallString := ""
	pkgPostinstall, ok := pkgHooks.RawGetString("postinstall").(lua.LString)
	if ok {
		pkgPreinstallString = pkgPostinstall.String()
	}

	pkgPreremoveString := ""
	pkgPreremove, ok := pkgHooks.RawGetString("preremove").(lua.LString)
	if ok {
		pkgPreremoveString = pkgPreremove.String()
	}

	pkgPostremoveString := ""
	pkgPostremove, ok := pkgHooks.RawGetString("postremove").(lua.LString)
	if ok {
		pkgPostremoveString = pkgPostremove.String()
	}

	pkg := apkg.PackageRoot{
		Spec: 1,
		Package: apkg.Package{
			Name:        pkgName.String(),
			Version:     pkgVersion.String(),
			Description: pkgDescription.String(),
			Authors:     pkgAuthorsList,
			Maintainers: pkgMaintainersList,
		},
		Dependencies: apkg.Dependencies{
			Required: pkgRequiredDependenciesList,
			Optional: pkgOptionalDependenciesList,
		},
		Files: pkgFilesMap,
		Hooks: apkg.Hooks{
			Preinstall:  pkgPreinstallString,
			Postinstall: pkgPostinstallString,
			Preremove:   pkgPreremoveString,
			Postremove:  pkgPostremoveString,
		},
	}

	var packageFileBuffer bytes.Buffer

	encoder := toml.NewEncoder(&packageFileBuffer)

	if err := encoder.Encode(pkg); err != nil {
		return err
	}

	ioutil.WriteFile(filepath.Join(name, "/pkg", "package.toml"), packageFileBuffer.Bytes(), 0777)

	if err := os.Chdir(filepath.Join(name, "/pkg")); err != nil {
		return err
	}

	cmd := exec.Command("tar", "--zstd", "-cf", filepath.Join(curr, c.Args().Get(1)), ".")

	if err := cmd.Run(); err != nil {
		return err
	}

	if err := os.Chdir(curr); err != nil {
		return err
	}

	return nil
}
