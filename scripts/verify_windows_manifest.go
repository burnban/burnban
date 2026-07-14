//go:build ignore

// verify_windows_manifest checks release executables for a real RT_MANIFEST
// resource instead of accepting a matching string elsewhere in the binary.
package main

import (
	"archive/zip"
	"bytes"
	"debug/pe"
	"encoding/binary"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	resourceDirectoryIndex = 2
	resourceTypeManifest   = 24
	resourceSubdirectory   = uint32(1 << 31)
	maxResourceEntries     = 4096
)

type resourceEntry struct {
	id          uint32
	isNamed     bool
	offset      uint32
	isDirectory bool
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: go run scripts/verify_windows_manifest.go <windows executable or zip>...")
		os.Exit(2)
	}

	for _, path := range os.Args[1:] {
		if err := verifyArtifact(path); err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", path, err)
			os.Exit(1)
		}
		fmt.Printf("%s: asInvoker manifest verified\n", path)
	}
}

func verifyArtifact(path string) error {
	executable, err := readExecutable(path)
	if err != nil {
		return err
	}

	image, err := pe.NewFile(bytes.NewReader(executable))
	if err != nil {
		return fmt.Errorf("parse PE executable: %w", err)
	}
	defer image.Close()

	if expected, ok := expectedMachine(path); ok && image.Machine != expected {
		return fmt.Errorf("machine is %#x, want %#x", image.Machine, expected)
	}

	rootRVA, err := resourceRootRVA(image)
	if err != nil {
		return err
	}
	rootEntries, err := readResourceDirectory(image, rootRVA, 0)
	if err != nil {
		return fmt.Errorf("read root resource directory: %w", err)
	}

	for _, entry := range rootEntries {
		if entry.isNamed || entry.id != resourceTypeManifest {
			continue
		}
		if !entry.isDirectory {
			return fmt.Errorf("RT_MANIFEST entry is not a resource directory")
		}
		manifests, err := readResourceLeaves(image, rootRVA, entry.offset, 0, make(map[uint32]bool))
		if err != nil {
			return fmt.Errorf("read RT_MANIFEST resource: %w", err)
		}
		for _, manifest := range manifests {
			ok, err := hasAsInvokerExecutionLevel(manifest)
			if err != nil {
				return fmt.Errorf("parse RT_MANIFEST XML: %w", err)
			}
			if ok {
				return nil
			}
		}
		return fmt.Errorf("RT_MANIFEST does not request asInvoker with uiAccess=false")
	}

	return fmt.Errorf("RT_MANIFEST resource is missing")
}

func readExecutable(path string) ([]byte, error) {
	if !strings.EqualFold(filepath.Ext(path), ".zip") {
		return os.ReadFile(path)
	}

	archive, err := zip.OpenReader(path)
	if err != nil {
		return nil, fmt.Errorf("open zip: %w", err)
	}
	defer archive.Close()

	for _, file := range archive.File {
		if !strings.EqualFold(filepath.Base(file.Name), "burnban.exe") {
			continue
		}
		reader, err := file.Open()
		if err != nil {
			return nil, fmt.Errorf("open %s: %w", file.Name, err)
		}
		data, readErr := io.ReadAll(reader)
		closeErr := reader.Close()
		if readErr != nil {
			return nil, fmt.Errorf("read %s: %w", file.Name, readErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close %s: %w", file.Name, closeErr)
		}
		return data, nil
	}

	return nil, fmt.Errorf("zip does not contain burnban.exe")
}

func expectedMachine(path string) (uint16, bool) {
	name := strings.ToLower(filepath.Base(path))
	switch {
	case strings.Contains(name, "amd64"):
		return pe.IMAGE_FILE_MACHINE_AMD64, true
	case strings.Contains(name, "arm64"):
		return pe.IMAGE_FILE_MACHINE_ARM64, true
	default:
		return 0, false
	}
}

func resourceRootRVA(image *pe.File) (uint32, error) {
	var directory pe.DataDirectory
	switch header := image.OptionalHeader.(type) {
	case *pe.OptionalHeader32:
		directory = header.DataDirectory[resourceDirectoryIndex]
	case *pe.OptionalHeader64:
		directory = header.DataDirectory[resourceDirectoryIndex]
	default:
		return 0, fmt.Errorf("unsupported PE optional header %T", image.OptionalHeader)
	}
	if directory.VirtualAddress == 0 || directory.Size == 0 {
		return 0, fmt.Errorf("PE resource directory is missing")
	}
	return directory.VirtualAddress, nil
}

func readResourceDirectory(image *pe.File, rootRVA, offset uint32) ([]resourceEntry, error) {
	header, err := readRVA(image, rootRVA+offset, 16)
	if err != nil {
		return nil, err
	}
	named := uint32(binary.LittleEndian.Uint16(header[12:14]))
	identified := uint32(binary.LittleEndian.Uint16(header[14:16]))
	count := named + identified
	if count > maxResourceEntries {
		return nil, fmt.Errorf("resource directory contains %d entries", count)
	}
	data, err := readRVA(image, rootRVA+offset+16, count*8)
	if err != nil {
		return nil, err
	}
	entries := make([]resourceEntry, 0, count)
	for i := uint32(0); i < count; i++ {
		start := i * 8
		name := binary.LittleEndian.Uint32(data[start : start+4])
		target := binary.LittleEndian.Uint32(data[start+4 : start+8])
		entries = append(entries, resourceEntry{
			id:          name &^ resourceSubdirectory,
			isNamed:     name&resourceSubdirectory != 0,
			offset:      target &^ resourceSubdirectory,
			isDirectory: target&resourceSubdirectory != 0,
		})
	}
	return entries, nil
}

func readResourceLeaves(image *pe.File, rootRVA, directoryOffset uint32, depth int, seen map[uint32]bool) ([][]byte, error) {
	if depth > 8 {
		return nil, fmt.Errorf("resource directory nesting exceeds limit")
	}
	if seen[directoryOffset] {
		return nil, fmt.Errorf("resource directory cycle at offset %#x", directoryOffset)
	}
	seen[directoryOffset] = true
	defer delete(seen, directoryOffset)

	entries, err := readResourceDirectory(image, rootRVA, directoryOffset)
	if err != nil {
		return nil, err
	}
	var leaves [][]byte
	for _, entry := range entries {
		if entry.isDirectory {
			nested, err := readResourceLeaves(image, rootRVA, entry.offset, depth+1, seen)
			if err != nil {
				return nil, err
			}
			leaves = append(leaves, nested...)
			continue
		}

		dataEntry, err := readRVA(image, rootRVA+entry.offset, 16)
		if err != nil {
			return nil, err
		}
		dataRVA := binary.LittleEndian.Uint32(dataEntry[0:4])
		size := binary.LittleEndian.Uint32(dataEntry[4:8])
		data, err := readRVA(image, dataRVA, size)
		if err != nil {
			return nil, err
		}
		leaves = append(leaves, data)
	}
	return leaves, nil
}

func readRVA(image *pe.File, rva, size uint32) ([]byte, error) {
	for _, section := range image.Sections {
		start := section.VirtualAddress
		if rva < start {
			continue
		}
		offset := uint64(rva - start)
		data, err := section.Data()
		if err != nil {
			return nil, err
		}
		end := offset + uint64(size)
		if end > uint64(len(data)) {
			continue
		}
		return data[offset:end], nil
	}
	return nil, fmt.Errorf("RVA %#x with size %d is outside file-backed sections", rva, size)
}

func hasAsInvokerExecutionLevel(manifest []byte) (bool, error) {
	decoder := xml.NewDecoder(bytes.NewReader(bytes.TrimRight(manifest, "\x00")))
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		start, ok := token.(xml.StartElement)
		if !ok || start.Name.Local != "requestedExecutionLevel" {
			continue
		}
		var level, uiAccess string
		for _, attribute := range start.Attr {
			switch attribute.Name.Local {
			case "level":
				level = attribute.Value
			case "uiAccess":
				uiAccess = attribute.Value
			}
		}
		return level == "asInvoker" && strings.EqualFold(uiAccess, "false"), nil
	}
}
