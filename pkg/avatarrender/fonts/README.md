# Avatar rendering font

## Files

| File | Purpose |
|---|---|
| `NotoSansCJKsc-Bold-cjk-subset.otf` | Font used for rendering (subsetted, see below) |
| `OFL.txt` | Full SIL Open Font License 1.1 text |

## Source

- **Font**: Noto Sans CJK SC (Source Han Sans, Simplified Chinese regional variant), Bold weight
- **Download**: `https://raw.githubusercontent.com/notofonts/noto-cjk/main/Sans/OTF/SimplifiedChinese/NotoSansCJKsc-Bold.otf`
- **License**: SIL Open Font License 1.1 — commercial use, bundling, and subsetting are permitted. The `Sans/LICENSE` file in `notofonts/noto-cjk` is the OFL 1.1 template with no Reserved Font Name declared, so redistributing under the original name is compliant.

## Subsetting

The full font is 16.2 MB; subsetted to **Chinese + Japanese + Korean coverage** it is 12.8 MB.

Retained Unicode ranges:
- Latin / punctuation: `U+0020-007E,U+00A0-024F,U+2000-206F,U+3000-303F,U+FF00-FFEF`
- Kana: `U+3040-30FF,U+31F0-31FF`
- CJK Unified Ideographs (full basic block): `U+4E00-9FFF`
- Hangul (full syllables + jamo): `U+AC00-D7A3,U+1100-11FF,U+3130-318F`

Dropped: CJK Extension A/B+, Chữ Nôm, and other scripts.

> The **entire** CJK basic block is kept intentionally (not trimmed to a "common
> characters" subset). Nicknames may contain rare or personal-name characters
> that cannot be predicted in advance; trimming would render those as tofu (□),
> which is especially likely for the international edition.

### Regenerate the subset

```sh
python3 -m fontTools.subset NotoSansCJKsc-Bold.otf \
  --unicodes="U+0020-007E,U+00A0-024F,U+2000-206F,U+3000-303F,U+FF00-FFEF,U+3040-30FF,U+31F0-31FF,U+4E00-9FFF,U+AC00-D7A3,U+1100-11FF,U+3130-318F" \
  --output-file=NotoSansCJKsc-Bold-cjk-subset.otf
```
