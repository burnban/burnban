# Third-party software notices

Burnban is MIT licensed. Its statically linked executable also contains the
following runtime dependencies. Versions are locked by `go.mod` and `go.sum`.

| Module | Version | License family |
|---|---:|---|
| `github.com/dustin/go-humanize` | v1.0.1 | MIT |
| `github.com/google/uuid` | v1.6.0 | BSD-3-Clause |
| `github.com/mattn/go-isatty` | v0.0.20 | MIT |
| `github.com/ncruces/go-strftime` | v1.0.0 | MIT |
| `github.com/remyoudompheng/bigfft` | 24d4a6f8daec | BSD-3-Clause |
| `golang.org/x/sys` | v0.44.0 | BSD-3-Clause |
| `modernc.org/libc` | v1.73.4 | BSD-3-Clause and bundled third-party terms |
| `modernc.org/mathutil` | v1.7.1 | BSD-3-Clause |
| `modernc.org/memory` | v1.11.0 | BSD-3-Clause and bundled third-party terms |
| `modernc.org/sqlite` | v1.53.0 | BSD-3-Clause; SQLite portions are public domain |

Official release archives and the container image include the exact upstream
license and notice files under `third_party_licenses/` or `/licenses`. Those
files are collected from the resolved module graph by
`scripts/collect_licenses.sh`, including `modernc.org/libc`'s
`LICENSE-3RD-PARTY.md` and the additional notices distributed with
`modernc.org/memory`.

Copyright remains with each upstream author and contributor. No endorsement by
an upstream project is implied. If this summary conflicts with an included
upstream license text, the upstream text controls.
