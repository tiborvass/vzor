package sbox

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"gvisor.googlesource.com/gvisor/pkg/abi/linux"
	"gvisor.googlesource.com/gvisor/pkg/cpuid"
	"gvisor.googlesource.com/gvisor/pkg/log"
	"gvisor.googlesource.com/gvisor/pkg/sentry/context"
	"gvisor.googlesource.com/gvisor/pkg/sentry/fs"
	host "gvisor.googlesource.com/gvisor/pkg/sentry/fs/host"
	"gvisor.googlesource.com/gvisor/pkg/sentry/fs/ramfs"
	"gvisor.googlesource.com/gvisor/pkg/sentry/kernel"
	"gvisor.googlesource.com/gvisor/pkg/sentry/kernel/auth"
	"gvisor.googlesource.com/gvisor/pkg/sentry/kernel/kdefs"
	"gvisor.googlesource.com/gvisor/pkg/sentry/limits"
	"gvisor.googlesource.com/gvisor/pkg/sentry/loader"
	"gvisor.googlesource.com/gvisor/pkg/sentry/memutil"
	"gvisor.googlesource.com/gvisor/pkg/sentry/pgalloc"
	"gvisor.googlesource.com/gvisor/pkg/sentry/socket/hostinet"
	slinux "gvisor.googlesource.com/gvisor/pkg/sentry/syscalls/linux"
	"gvisor.googlesource.com/gvisor/pkg/sentry/time"
	"gvisor.googlesource.com/gvisor/pkg/sentry/usage"

	_ "gvisor.googlesource.com/gvisor/pkg/sentry/fs/dev"
	_ "gvisor.googlesource.com/gvisor/pkg/sentry/fs/proc"
	_ "gvisor.googlesource.com/gvisor/pkg/sentry/fs/sys"
	_ "gvisor.googlesource.com/gvisor/pkg/sentry/fs/tmpfs"
	_ "gvisor.googlesource.com/gvisor/pkg/sentry/fs/tty"
)

type Opt struct {
	HostNet bool
	TTY     bool
	Mounts  string
	Args    []string
}

func Run(o Opt) error {
	log.SetLevel(log.Warning)

	// Register the global syscall table.
	kernel.RegisterSyscallTable(slinux.AMD64)

	if err := usage.Init(); err != nil {
		return fmt.Errorf("error setting up memory usage: %v", err)
	}

	p, err := newPlatform()
	if err != nil {
		return err
	}

	k := &kernel.Kernel{
		Platform: p,
	}

	// Create memory file.
	mf, err := createMemoryFile()
	if err != nil {
		return fmt.Errorf("creating memory file: %v", err)
	}
	k.SetMemoryFile(mf)

	vdso, err := loader.PrepareVDSO(k)
	if err != nil {
		return fmt.Errorf("error creating vdso: %v", err)
	}

	tk, err := kernel.NewTimekeeper(k, vdso.ParamPage.FileRange())
	if err != nil {
		return fmt.Errorf("error creating timekeeper: %v", err)
	}
	tk.SetClocks(time.NewCalibratedClocks())

	networkStack, err := netStack(k, o.HostNet)
	if err != nil {
		return err
	}

	stack, ok := networkStack.(*hostinet.Stack)
	if ok {
		if err := stack.Configure(); err != nil {
			return err
		}
	}

	creds := auth.NewUserCredentials(
		auth.KUID(0),
		auth.KGID(0),
		nil,
		nil,
		auth.NewRootUserNamespace())

	if err = k.Init(kernel.InitKernelArgs{
		FeatureSet:                  cpuid.HostFeatureSet(),
		Timekeeper:                  tk,
		RootUserNamespace:           creds.UserNamespace,
		NetworkStack:                networkStack,
		ApplicationCores:            uint(runtime.NumCPU()),
		Vdso:                        vdso,
		RootUTSNamespace:            kernel.NewUTSNamespace("sbox", "", creds.UserNamespace),
		RootIPCNamespace:            kernel.NewIPCNamespace(creds.UserNamespace),
		RootAbstractSocketNamespace: kernel.NewAbstractSocketNamespace(),
	}); err != nil {
		return fmt.Errorf("error initializing kernel: %v", err)
	}

	ls, err := limits.NewLinuxLimitSet()
	if err != nil {
		return err
	}

	// Create the process arguments.
	procArgs := kernel.CreateProcessArgs{
		Argv:                    o.Args,
		Envv:                    []string{},
		WorkingDirectory:        "/", // Defaults to '/' if empty.
		Credentials:             creds,
		Umask:                   0022,
		Limits:                  ls,
		MaxSymlinkTraversals:    linux.MaxSymlinkTraversals,
		UTSNamespace:            k.RootUTSNamespace(),
		IPCNamespace:            k.RootIPCNamespace(),
		AbstractSocketNamespace: k.RootAbstractSocketNamespace(),
		ContainerID:             "sbox",
	}
	ctx := procArgs.NewContext(k)

	fdm, err := createFDMap(ctx, k, ls, o.TTY, []int{0, 1, 2})
	if err != nil {
		return fmt.Errorf("error importing fds: %v", err)
	}
	// CreateProcess takes a reference on FDMap if successful. We
	// won't need ours either way.
	procArgs.FDMap = fdm

	rootProcArgs := kernel.CreateProcessArgs{
		WorkingDirectory:     "/",
		Credentials:          auth.NewRootCredentials(creds.UserNamespace),
		Umask:                0022,
		MaxSymlinkTraversals: linux.MaxSymlinkTraversals,
	}
	rootCtx := rootProcArgs.NewContext(k)

	mns := k.RootMountNamespace()
	if mns == nil {
		followLinks := uint(linux.MaxSymlinkTraversals)
		mns, err := createMountNamespace(ctx, rootCtx, strings.Split(o.Mounts, ","), &followLinks)
		if err != nil {
			return fmt.Errorf("error creating mounts: %v", err)
		}
		k.SetRootMountNamespace(mns)
	}
	_, _, err = k.CreateProcess(procArgs)
	if err != nil {
		return fmt.Errorf("failed to create init process: %v", err)
	}

	tg := k.GlobalInit()
	if o.TTY {
		ttyFile := procArgs.FDMap.GetFile(0)
		defer ttyFile.DecRef()
		ttyfop := ttyFile.FileOperations.(*host.TTYFileOperations)
		// Set the foreground process group on the TTY to the global
		// init process group, since that is what we are about to
		// start running.
		ttyfop.InitForegroundProcessGroup(tg.ProcessGroup())
	}

	if err := k.Start(); err != nil {
		return err
	}

	k.WaitExited()

	return nil
}

func addSubmountOverlay(ctx context.Context, inode *fs.Inode, submounts []string) (*fs.Inode, error) {
	// There is no real filesystem backing this ramfs tree, so we pass in
	// "nil" here.
	msrc := fs.NewNonCachingMountSource(nil, fs.MountSourceFlags{})
	mountTree, err := ramfs.MakeDirectoryTree(ctx, msrc, submounts)
	if err != nil {
		return nil, fmt.Errorf("error creating mount tree: %v", err)
	}
	overlayInode, err := fs.NewOverlayRoot(ctx, inode, mountTree, fs.MountSourceFlags{})
	if err != nil {
		return nil, fmt.Errorf("failed to make mount overlay: %v", err)
	}
	return overlayInode, err
}

func createMountNamespace(userCtx context.Context, rootCtx context.Context, mounts []string, maxTraversals *uint) (*fs.MountNamespace, error) {
	rootInode, err := createRootMount(rootCtx, mounts)
	if err != nil {
		return nil, fmt.Errorf("failed to create root mount: %v", err)
	}

	mns, err := fs.NewMountNamespace(userCtx, rootInode)
	if err != nil {
		return nil, fmt.Errorf("failed to create root mount namespace: %v", err)
	}

	root := mns.Root()
	defer root.DecRef()

	proc, ok := fs.FindFilesystem("proc")
	if !ok {
		panic(fmt.Sprintf("could not find filesystem proc"))
	}
	ctx := rootCtx
	inode, err := proc.Mount(ctx, "none", fs.MountSourceFlags{}, "", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create mount with source: %v", err)
	}

	dirent, err := mns.FindInode(ctx, root, root, "/proc", maxTraversals)
	if err != nil {
		return nil, fmt.Errorf("failed to find mount destination: %v", err)
	}
	defer dirent.DecRef()
	if err := mns.Mount(ctx, dirent, inode); err != nil {
		return nil, fmt.Errorf("failed to mount at destination: %v", err)
	}

	return mns, nil
}

func createRootMount(ctx context.Context, mounts []string) (*fs.Inode, error) {
	// First construct the filesystem from the spec.Root.
	mf := fs.MountSourceFlags{ReadOnly: false}

	var (
		rootInode, prevInode *fs.Inode
		err                  error
	)

	wd, err := os.Getwd()
	if err != nil {
		return nil, err
	}

	host, ok := fs.FindFilesystem("whitelistfs")
	if !ok {
		panic(fmt.Sprintf("could not find filesystem host"))
	}
	for i, m := range mounts {
		if !filepath.IsAbs(m) {
			m = filepath.Join(wd, m)
		}
		// fmt.Println("root=" + m)
		rootInode, err = host.Mount(ctx, "", mf, "root="+m, nil)
		if err != nil {
			return nil, fmt.Errorf("failed to generate root mount point: %v", err)
		}
		if i != 0 {
			rootInode, err = fs.NewOverlayRoot(ctx, rootInode, prevInode, fs.MountSourceFlags{})
			if err != nil {
				return nil, fmt.Errorf("failed to make mount overlay: %v", err)
			}
		}
		prevInode = rootInode
	}

	submounts := []string{"/dev", "/sys", "/proc", "/tmp"}
	rootInode, err = addSubmountOverlay(ctx, rootInode, submounts)
	if err != nil {
		return nil, fmt.Errorf("error adding submount overlay: %v", err)
	}

	tmpfs, ok := fs.FindFilesystem("tmpfs")
	if !ok {
		panic(fmt.Sprintf("could not find filesystem tmpfs"))
	}

	upper, err := tmpfs.Mount(ctx, "upper", fs.MountSourceFlags{}, "", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create tmpfs overlay: %v", err)
	}
	rootInode, err = fs.NewOverlayRoot(ctx, upper, rootInode, fs.MountSourceFlags{})
	if err != nil {
		return nil, fmt.Errorf("failed to make mount overlay: %v", err)
	}

	return rootInode, nil
}

func createFDMap(ctx context.Context, k *kernel.Kernel, l *limits.LimitSet, console bool, stdioFDs []int) (*kernel.FDMap, error) {
	if len(stdioFDs) != 3 {
		return nil, fmt.Errorf("stdioFDs should contain exactly 3 FDs (stdin, stdout, and stderr), but %d FDs received", len(stdioFDs))
	}

	fdm := k.NewFDMap()
	defer fdm.DecRef()
	mounter := fs.FileOwnerFromContext(ctx)

	// Maps sandbox FD to host FD.
	fdMap := map[int]int{
		0: stdioFDs[0],
		1: stdioFDs[1],
		2: stdioFDs[2],
	}

	var ttyFile *fs.File
	for appFD, hostFD := range fdMap {
		var appFile *fs.File

		if console && appFD < 3 {
			// Import the file as a host TTY file.
			if ttyFile == nil {
				var err error
				appFile, err = host.ImportFile(ctx, hostFD, mounter, true /* isTTY */)
				if err != nil {
					return nil, err
				}
				defer appFile.DecRef()

				// Remember this in the TTY file, as we will
				// use it for the other stdio FDs.
				ttyFile = appFile
			} else {
				// Re-use the existing TTY file, as all three
				// stdio FDs must point to the same fs.File in
				// order to share TTY state, specifically the
				// foreground process group id.
				appFile = ttyFile
			}
		} else {
			// Import the file as a regular host file.
			var err error
			appFile, err = host.ImportFile(ctx, hostFD, mounter, false /* isTTY */)
			if err != nil {
				return nil, err
			}
			defer appFile.DecRef()
		}

		// Add the file to the FD map.
		if err := fdm.NewFDAt(kdefs.FD(appFD), appFile, kernel.FDFlags{}, l); err != nil {
			return nil, err
		}
	}

	fdm.IncRef()
	return fdm, nil
}

func createMemoryFile() (*pgalloc.MemoryFile, error) {
	const memfileName = "runsc-memory"
	memfd, err := memutil.CreateMemFD(memfileName, 0)
	if err != nil {
		return nil, fmt.Errorf("error creating memfd: %v", err)
	}
	memfile := os.NewFile(uintptr(memfd), memfileName)
	mf, err := pgalloc.NewMemoryFile(memfile, pgalloc.MemoryFileOpts{})
	if err != nil {
		memfile.Close()
		return nil, fmt.Errorf("error creating pgalloc.MemoryFile: %v", err)
	}
	return mf, nil
}
