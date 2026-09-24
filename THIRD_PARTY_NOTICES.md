# Third-Party License Notices

This document summarizes the licenses of PBMT's dependencies and the notices to retain when distributing them. It does not assign a license to PBMT's own code. Versions refer to [go.mod](source_code/go.mod) and the bundled sources.

This is a license overview, not a bundle of the full license texts. The applicable upstream license texts and attribution notices must accompany a release.

## Main Dependencies

| Component | License | What must be considered |
| --- | --- | --- |
| Bundled `libiec61850` v1.6.2 | GPL-3.0-or-later; separate commercial licensing available | Compiled into PBMT through CGO. Preserve its notices and [COPYING](source_code/deps/libiec61850/COPYING); distribution under the GPL requires the combined application to meet GPL terms, including Corresponding Source obligations. See [upstream licensing](https://libiec61850.com/about/). |
| Fyne v2.7.3 | BSD-3-Clause | Preserve copyright, license conditions, and disclaimer in source and binary distributions; do not imply upstream endorsement. [License](https://github.com/fyne-io/fyne/blob/v2.7.3/LICENSE). |
| `github.com/google/gopacket` v1.1.19 | BSD-3-Clause | Preserve copyright, conditions, and disclaimer; also account for the native `libpcap` library. [License](https://github.com/google/gopacket/blob/v1.1.19/LICENSE). |
| `golang.org/x/net` v0.35.0 and `golang.org/x/sys` v0.41.0 | BSD-3-Clause | Preserve the Go Authors' notices, license conditions, and disclaimer. The other listed `golang.org/x/*` modules use the same license family. [net license](https://github.com/golang/net/blob/v0.35.0/LICENSE), [sys license](https://github.com/golang/sys/blob/v0.41.0/LICENSE). |
| `gopkg.in/yaml.v3` v3.0.1 | MIT and Apache-2.0, applying to different files | Retain both license texts and the upstream `NOTICE`; this is not a choice of either license for the whole library. [License](https://github.com/go-yaml/yaml/blob/v3.0.1/LICENSE), [NOTICE](https://github.com/go-yaml/yaml/blob/v3.0.1/NOTICE). |
| MCP Go SDK v1.8.0 | Apache-2.0 plus MIT for contributions not relicensed | This version is in a licensing transition. Preserve its complete license text and applicable notices. Upstream documentation has separate CC-BY-4.0 terms if redistributed. [Version-specific license](https://github.com/modelcontextprotocol/go-sdk/blob/v1.8.0/LICENSE). |

## Transitive Libraries, Native Code, and Assets

The direct Go dependencies do not cover every notice that a release needs:

- **MIT:** examples include `google/jsonschema-go`, `segmentio/asm`, `segmentio/encoding`, `BurntSushi/toml`, `go-gl/gl`, `go-i18n`, and `goldmark`. Retain their copyright and permission notices.
- **BSD:** examples include `fsnotify`, `godbus/dbus`, the Go GLFW bindings, `rasterx`, and `oksvg`. Preserve the exact license and disclaimer supplied by each module. `go-text/render` and `go-text/typesetting` offer **Unlicense OR BSD-3-Clause**; the latter's HarfBuzz code also carries separate MIT notices.
- **Apache-2.0:** examples include `fyne.io/systray`, `rymdport/portal`, `hack-pad/go-indexeddb`, and `hack-pad/safejs`. Include the license and relevant upstream `NOTICE` contents where present, retain attribution, and mark modified files as required by [Apache-2.0 section 4](https://www.apache.org/licenses/LICENSE-2.0#redistribution).
- **ISC:** `nfnt/resize` and `davecgh/go-spew` require retention of copyright and permission notices. Some modules in `go.mod` are test-only or platform-specific; inclusion depends on the shipped source or binary.
- **Native libraries:** `libpcap` has a [BSD-style license](https://github.com/the-tcpdump-group/libpcap/blob/master/LICENSE); use the notices from the version actually shipped. GLFW's bundled C source uses the [zlib/libpng license](https://github.com/go-gl/glfw/blob/037f3cc74f2a/v3.3/glfw/glfw/LICENSE.md), separately from its Go bindings. Preserve notices and identify source modifications. Bundled ASN.1 support inside `libiec61850` also contains BSD notices in individual files.
- **Fonts:** Fyne bundles fonts with [SIL OFL-1.1](https://github.com/fyne-io/fyne/blob/v2.7.3/theme/font/LICENSE.txt), [Inter](https://github.com/fyne-io/fyne/blob/v2.7.3/theme/font/LICENSE_Inter.txt), and [Bitstream Vera/Arev notices for DejaVu-Powerline](https://github.com/fyne-io/fyne/blob/v2.7.3/theme/font/LICENSE_DejaVu-Powerline.txt). Include the applicable texts and comply with font naming/modification restrictions. Fyne's BSD license alone does not cover these assets.

## Distribution

For the current build using the GPL version of `libiec61850`, plan a GPLv3-compatible distribution of the combined application: include PBMT's top-level `LICENSE`, retain third-party notices, and provide recipients with Corresponding Source, including relevant modifications and build scripts, through a method permitted by GPLv3. Private use does not by itself require publishing the code to the public. See the [GNU GPL FAQ](https://www.gnu.org/licenses/gpl-faq.html#GPLStaticVsDynamic) and [GPLv3 sections 5–6](https://www.gnu.org/licenses/gpl-3.0.html).

A commercial `libiec61850` license may replace that library's GPL requirements within the agreement's scope. Other dependencies' obligations must also be addressed before a proprietary distribution.

Ship the actual applicable license texts and attribution notices with a release, for example in a `licenses/` directory alongside this overview. This overview and its links are not a substitute for those materials or a complete license inventory of a platform-specific release. The provenance of PBMT's own implementation has not been audited here.
