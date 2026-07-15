package main

// Keep the generated COFF resources architecture-specific so Go links the
// matching application manifest into each Windows executable.
//go:generate go run github.com/akavel/rsrc@v0.10.2 -arch amd64 -manifest burnban.exe.manifest -o rsrc_windows_amd64.syso
//go:generate go run github.com/akavel/rsrc@v0.10.2 -arch arm64 -manifest burnban.exe.manifest -o rsrc_windows_arm64.syso
