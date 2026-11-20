# Third-party licenses

- `aac-go-LICENSE.txt`: [github.com/arabian9ts/aac-go v0.1.0](https://github.com/arabian9ts/aac-go), used as a pure-Go AAC-LC decoding fallback. Licensed under the Apache License 2.0.
- `shinonome-LICENSE.txt`: [Shinonome font family 0.9.11](http://openlab.ring.gr.jp/efont/shinonome/) by The Electronic Font Open Laboratory (/efont/). Its 16-dot gothic JIS X 0208 font and half-width katakana are embedded, in a converted form (`dvd_menu_font_shinonome16.bin`, built by `gen/menufont`), to draw Japanese text in the title menu. The font is in the public domain; the file holds its license and its list of authors, converted to UTF-8.
- `x-image-LICENSE.txt`: [golang.org/x/image v0.48.0](https://pkg.go.dev/golang.org/x/image), whose `font/sfnt` package reads the TrueType or OpenType font of `Options.MenuFont`. Licensed under the BSD 3-Clause License. The same license, with the same text, covers [golang.org/x/text](https://pkg.go.dev/golang.org/x/text), which `font/sfnt` uses.

The `go-vp9` module was already a dependency of this project and is now used for both supported VP9 profiles. Its BSD 2-Clause license and attribution notice are distributed with the module source; this project does not copy its implementation into this repository.
