# Changelog

## 0.2.0

- `LOOP` clause: `LOOP` and `UNTIL` keywords, plus `EVERY` and `MAX`
  highlighted as keywords (they are contextual words inside a LOOP in
  the chassis parser, so a `WITH max = …` key is unaffected by the
  language, but the grammar colors them everywhere for simplicity).
- Snippets: `loop` (drain a cursor inside one op) and `poll` (call an
  endpoint until it says done).

## 0.1.2

- Marketplace listing polish: concise README, marketplace icon, Snippets
  category.

## 0.1.1

- File icon for `.txcl` in the Explorer (the three-circle brand mark).
- Marketplace icon.
- MIT license.
- Requires VS Code 1.86+ (for the `languages.icon` contribution).

## 0.1.0

- Initial release: TextMate grammar, language configuration, snippets.

- TextMate grammar covering comments, strings (incl. `b64"..."` prefix),
  regex-after-binding literals, numbers, branch paths (`.x.y.z` and `@x`
  sugar), the `&fn` function-call sigil, the case-flexible keyword set
  from `chassis/txcl/token/token.go`, booleans/null, operators, and
  punctuation.
- `language-configuration.json` with `#` line comments, bracket
  matching, auto-closing pairs.
- Five snippets: `when`, `set`, `exec`, `emit`, `rule`.
