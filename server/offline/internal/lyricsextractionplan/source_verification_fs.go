package lyricsextractionplan

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
)

func openSourceTree(root string) (*sourceTree, error) {
	absoluteRoot, err := directVerificationRoot(root)
	if err != nil {
		return nil, err
	}
	rootHandle, err := os.OpenRoot(absoluteRoot)
	if err != nil {
		return nil, fmt.Errorf("open verification root descriptor: %w", err)
	}
	info, err := rootHandle.Stat(".")
	if err != nil {
		rootHandle.Close()
		return nil, fmt.Errorf("inspect verification root descriptor: %w", err)
	}
	lexical, err := os.Lstat(absoluteRoot)
	if err != nil {
		rootHandle.Close()
		return nil, fmt.Errorf("reinspect verification root: %w", err)
	}
	policy := CompiledRecoverySourceSelectionPolicy()
	if err := validateRecoverySourceSelectionPolicy(policy); err != nil {
		rootHandle.Close()
		return nil, err
	}
	if err := validateSourceDirectoryInfo(info, policy); err != nil {
		rootHandle.Close()
		return nil, fmt.Errorf("verification root policy: %w", err)
	}
	if err := sameExactSourceInfo(lexical, info); err != nil {
		rootHandle.Close()
		return nil, fmt.Errorf("verification root descriptor identity: %w", err)
	}
	return &sourceTree{
		absoluteRoot: absoluteRoot, root: rootHandle, rootInfo: info, policy: policy,
	}, nil
}

func (tree *sourceTree) Close() error {
	return tree.root.Close()
}

func (tree *sourceTree) hashFile(relativePath string, maximum int64, collect bool, hook sourceReadHook) (verifiedSourceFile, error) {
	state, err := tree.inspectPath(relativePath, maximum)
	if err != nil {
		return verifiedSourceFile{}, err
	}
	if hook != nil {
		if err := hook(sourceReadAfterPathInspection, relativePath); err != nil {
			return verifiedSourceFile{}, err
		}
	}

	segments := strings.Split(relativePath, "/")
	currentRoot := tree.root
	openedRoots := make([]*os.Root, 0, len(segments)-1)
	closeRoots := func() {
		for index := len(openedRoots) - 1; index >= 0; index-- {
			_ = openedRoots[index].Close()
		}
	}
	for index, segment := range segments[:len(segments)-1] {
		child, err := currentRoot.OpenRoot(filepath.FromSlash(segment))
		if err != nil {
			closeRoots()
			return verifiedSourceFile{}, fmt.Errorf("open parent directory descriptor: %w", err)
		}
		openedRoots = append(openedRoots, child)
		info, err := child.Stat(".")
		if err != nil {
			closeRoots()
			return verifiedSourceFile{}, fmt.Errorf("inspect parent directory descriptor: %w", err)
		}
		if err := sameExactSourceInfo(state.Components[index].Info, info); err != nil {
			closeRoots()
			return verifiedSourceFile{}, fmt.Errorf("parent directory identity changed while opening: %w", err)
		}
		currentRoot = child
	}
	file, err := currentRoot.Open(filepath.FromSlash(segments[len(segments)-1]))
	if err != nil {
		closeRoots()
		return verifiedSourceFile{}, fmt.Errorf("open source file descriptor: %w", err)
	}
	defer file.Close()
	defer closeRoots()
	openedInfo, err := file.Stat()
	if err != nil {
		return verifiedSourceFile{}, fmt.Errorf("inspect opened source file: %w", err)
	}
	leafIndex := len(state.Components) - 1
	if err := sameExactSourceInfo(state.Components[leafIndex].Info, openedInfo); err != nil {
		return verifiedSourceFile{}, fmt.Errorf("source descriptor/open identity changed: %w", err)
	}
	if err := validateSourceFileInfo(openedInfo, maximum, tree.policy); err != nil {
		return verifiedSourceFile{}, err
	}
	if hook != nil {
		if err := hook(sourceReadAfterOpen, relativePath); err != nil {
			return verifiedSourceFile{}, err
		}
	}

	digest := sha256.New()
	var body bytes.Buffer
	buffer := make([]byte, 64<<10)
	var readBytes int64
	firstChunk := true
	for {
		read, readErr := file.Read(buffer)
		if read > 0 {
			if int64(read) > maximum-readBytes {
				return verifiedSourceFile{}, fmt.Errorf("source file exceeds %d bytes", maximum)
			}
			readBytes += int64(read)
			_, _ = digest.Write(buffer[:read])
			if collect {
				_, _ = body.Write(buffer[:read])
			}
			if firstChunk && hook != nil {
				firstChunk = false
				if err := hook(sourceReadAfterFirstChunk, relativePath); err != nil {
					return verifiedSourceFile{}, err
				}
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return verifiedSourceFile{}, fmt.Errorf("hash source file: %w", readErr)
		}
	}
	if readBytes != openedInfo.Size() {
		return verifiedSourceFile{}, fmt.Errorf("source size changed while hashing: got %d, opened %d", readBytes, openedInfo.Size())
	}
	finalInfo, err := file.Stat()
	if err != nil {
		return verifiedSourceFile{}, fmt.Errorf("inspect hashed source file: %w", err)
	}
	if err := sameExactSourceInfo(openedInfo, finalInfo); err != nil {
		return verifiedSourceFile{}, fmt.Errorf("source file mutated while hashing: %w", err)
	}
	if hook != nil {
		if err := hook(sourceReadBeforeRevalidation, relativePath); err != nil {
			return verifiedSourceFile{}, err
		}
	}
	for index, openedRoot := range openedRoots {
		info, err := openedRoot.Stat(".")
		if err != nil {
			return verifiedSourceFile{}, fmt.Errorf("reinspect opened directory descriptor: %w", err)
		}
		if err := sameExactSourceInfo(state.Components[index].Info, info); err != nil {
			return verifiedSourceFile{}, fmt.Errorf("opened directory changed while hashing: %w", err)
		}
	}
	if err := tree.revalidatePathState(state); err != nil {
		return verifiedSourceFile{}, err
	}
	return verifiedSourceFile{
		Identity: SourceFileIdentity{
			Path: relativePath, SizeBytes: readBytes, SHA256: hex.EncodeToString(digest.Sum(nil)),
		},
		Body:  body.Bytes(),
		State: state,
	}, nil
}

func (tree *sourceTree) inspectPath(relativePath string, maximum int64) (sourcePathState, error) {
	if !validDataPath(relativePath) {
		return sourcePathState{}, errors.New("source path is not canonical")
	}
	segments := strings.Split(relativePath, "/")
	components := make([]sourcePathComponent, 0, len(segments))
	for index := range segments {
		current := strings.Join(segments[:index+1], "/")
		info, err := tree.lstatExact(current)
		if err != nil {
			return sourcePathState{}, err
		}
		component := sourcePathComponent{Path: current, Info: info, Directory: index < len(segments)-1}
		if component.Directory {
			if err := validateSourceDirectoryInfo(info, tree.policy); err != nil {
				return sourcePathState{}, fmt.Errorf("source parent %q: %w", current, err)
			}
		} else if err := validateSourceFileInfo(info, maximum, tree.policy); err != nil {
			return sourcePathState{}, fmt.Errorf("source file %q: %w", current, err)
		}
		components = append(components, component)
	}
	return sourcePathState{Path: relativePath, Components: components}, nil
}

func (tree *sourceTree) captureDirectory(relativePath string) (sourcePathComponent, error) {
	info, err := tree.lstatExact(relativePath)
	if err != nil {
		return sourcePathComponent{}, err
	}
	if err := validateSourceDirectoryInfo(info, tree.policy); err != nil {
		return sourcePathComponent{}, fmt.Errorf("source directory %q: %w", relativePath, err)
	}
	return sourcePathComponent{Path: relativePath, Info: info, Directory: true}, nil
}

func (tree *sourceTree) revalidatePathState(state sourcePathState) error {
	for _, component := range state.Components {
		if err := tree.revalidateComponent(component); err != nil {
			return err
		}
	}
	return tree.revalidateRoot()
}

func (tree *sourceTree) revalidateComponent(component sourcePathComponent) error {
	info, err := tree.lstatExact(component.Path)
	if err != nil {
		return err
	}
	if component.Directory {
		if err := validateSourceDirectoryInfo(info, tree.policy); err != nil {
			return err
		}
	} else if err := validateSourceFileInfo(info, tree.policy.MaximumFileBytes, tree.policy); err != nil {
		return err
	}
	return sameExactSourceInfo(component.Info, info)
}

func (tree *sourceTree) revalidateRoot() error {
	descriptorInfo, err := tree.root.Stat(".")
	if err != nil {
		return fmt.Errorf("reinspect verification root descriptor: %w", err)
	}
	lexicalInfo, err := os.Lstat(tree.absoluteRoot)
	if err != nil {
		return fmt.Errorf("reinspect verification root path: %w", err)
	}
	if err := validateSourceDirectoryInfo(descriptorInfo, tree.policy); err != nil {
		return err
	}
	if err := sameExactSourceInfo(tree.rootInfo, descriptorInfo); err != nil {
		return fmt.Errorf("verification root descriptor changed: %w", err)
	}
	if err := sameExactSourceInfo(tree.rootInfo, lexicalInfo); err != nil {
		return fmt.Errorf("verification root path changed: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(tree.absoluteRoot)
	if err != nil || resolved != tree.absoluteRoot {
		return errors.New("verification root acquired a symlink or filesystem alias")
	}
	return nil
}

func (tree *sourceTree) lstatExact(relativePath string) (os.FileInfo, error) {
	rootedInfo, err := tree.root.Lstat(filepath.FromSlash(relativePath))
	if err != nil {
		return nil, fmt.Errorf("inspect rooted path %q: %w", relativePath, err)
	}
	absolutePath := filepath.Join(tree.absoluteRoot, filepath.FromSlash(relativePath))
	lexicalInfo, err := os.Lstat(absolutePath)
	if err != nil {
		return nil, fmt.Errorf("inspect lexical path %q: %w", relativePath, err)
	}
	if err := sameExactSourceInfo(rootedInfo, lexicalInfo); err != nil {
		return nil, fmt.Errorf("rooted and lexical path %q disagree: %w", relativePath, err)
	}
	resolved, err := filepath.EvalSymlinks(absolutePath)
	if err != nil || resolved != absolutePath {
		return nil, fmt.Errorf("source path %q traverses a symlink or filesystem alias", relativePath)
	}
	return rootedInfo, nil
}

func validateSourceFileInfo(info os.FileInfo, maximum int64, policy RecoverySourceSelectionPolicy) error {
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("source must be a direct regular file")
	}
	if info.Size() < 0 || info.Size() > maximum {
		return fmt.Errorf("source file size %d is outside the bound 0..%d", info.Size(), maximum)
	}
	fingerprint, err := sourceFingerprint(info)
	if err != nil {
		return err
	}
	if fingerprint.UID != uint64(os.Geteuid()) {
		return errors.New("source file is not owned by the effective user")
	}
	if fingerprint.Nlink != 1 {
		return errors.New("source file must have exactly one filesystem link")
	}
	if !allowedSourceMode(uint32(info.Mode().Perm()), policy.AllowedFileModes) ||
		info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return fmt.Errorf("source file mode %04o is not allowed", info.Mode().Perm())
	}
	return nil
}

func validateSourceDirectoryInfo(info os.FileInfo, policy RecoverySourceSelectionPolicy) error {
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("source directory must be direct")
	}
	fingerprint, err := sourceFingerprint(info)
	if err != nil {
		return err
	}
	if fingerprint.UID != uint64(os.Geteuid()) {
		return errors.New("source directory is not owned by the effective user")
	}
	if !allowedSourceMode(uint32(info.Mode().Perm()), policy.AllowedDirectoryModes) ||
		info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return fmt.Errorf("source directory mode %04o is not allowed", info.Mode().Perm())
	}
	return nil
}

func allowedSourceMode(mode uint32, allowed []uint32) bool {
	for _, candidate := range allowed {
		if mode == candidate {
			return true
		}
	}
	return false
}

func sameExactSourceInfo(expected, actual os.FileInfo) error {
	if !os.SameFile(expected, actual) {
		return errors.New("filesystem object identity changed")
	}
	expectedFingerprint, err := sourceFingerprint(expected)
	if err != nil {
		return err
	}
	actualFingerprint, err := sourceFingerprint(actual)
	if err != nil {
		return err
	}
	if expectedFingerprint != actualFingerprint {
		return errors.New("filesystem object metadata changed")
	}
	return nil
}

func sourceFingerprint(info os.FileInfo) (sourceStatFingerprint, error) {
	value := reflect.ValueOf(info.Sys())
	if !value.IsValid() {
		return sourceStatFingerprint{}, errors.New("filesystem metadata is unavailable")
	}
	if value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return sourceStatFingerprint{}, errors.New("filesystem metadata is unavailable")
		}
		value = value.Elem()
	}
	uid, ok := numericStatField(value, "Uid")
	if !ok {
		return sourceStatFingerprint{}, errors.New("filesystem owner metadata is unavailable")
	}
	nlink, ok := numericStatField(value, "Nlink")
	if !ok {
		return sourceStatFingerprint{}, errors.New("filesystem link-count metadata is unavailable")
	}
	device, ok := numericStatField(value, "Dev")
	if !ok {
		return sourceStatFingerprint{}, errors.New("filesystem device metadata is unavailable")
	}
	inode, ok := numericStatField(value, "Ino")
	if !ok {
		return sourceStatFingerprint{}, errors.New("filesystem inode metadata is unavailable")
	}
	ctimeSec, ctimeNSec, ok := changeTimeStatFields(value)
	if !ok {
		return sourceStatFingerprint{}, errors.New("filesystem change-time metadata is unavailable")
	}
	return sourceStatFingerprint{
		Mode: info.Mode(), Size: info.Size(), ModTimeNS: info.ModTime().UnixNano(),
		UID: uid, Nlink: nlink, Device: device, Inode: inode, CTimeSec: ctimeSec, CTimeNSec: ctimeNSec,
	}, nil
}

func numericStatField(value reflect.Value, name string) (uint64, bool) {
	field := value.FieldByName(name)
	if !field.IsValid() {
		return 0, false
	}
	switch field.Kind() {
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return field.Uint(), true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if field.Int() < 0 {
			return 0, false
		}
		return uint64(field.Int()), true
	default:
		return 0, false
	}
}

func signedStatField(value reflect.Value, name string) (int64, bool) {
	field := value.FieldByName(name)
	if !field.IsValid() {
		return 0, false
	}
	switch field.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return field.Int(), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		if field.Uint() > uint64(^uint64(0)>>1) {
			return 0, false
		}
		return int64(field.Uint()), true
	default:
		return 0, false
	}
}

func changeTimeStatFields(value reflect.Value) (int64, int64, bool) {
	for _, name := range []string{"Ctim", "Ctimespec"} {
		field := value.FieldByName(name)
		if field.IsValid() {
			sec, secOK := signedStatField(field, "Sec")
			nsec, nsecOK := signedStatField(field, "Nsec")
			if secOK && nsecOK {
				return sec, nsec, true
			}
		}
	}
	sec, secOK := signedStatField(value, "Ctime")
	nsec, nsecOK := signedStatField(value, "Ctimensec")
	return sec, nsec, secOK && nsecOK
}
