The new-issue and edit modals use focus as the sole keyboard-ownership state.

- The title, body, flow, preset, depends-on, and attach fields share one
  rune-safe text-editor rule. Left and Right move within the current line;
  Up and Down move between body lines at the nearest available column. An
  arrow at a boundary remains owned by the editor as a no-op. Up and Down in
  a single-line field are also editor-owned no-ops.
- Typing inserts at the caret and Backspace removes the preceding rune. Tab
  moves through the existing fields and preserves each field's caret. A new
  or prefilled modal starts each caret at the end of its value.
- The priority field is a chooser, not a text input. `h`/`l` and Left/Right
  cycle priority only while it is focused; `h` and `l` remain ordinary text
  in editable fields. When no modal is open, existing panel arrow navigation
  is unchanged. Invalid modal focus is consumed without leaking input to the
  panel.
- Caret state is transient TUI state. Enter, Ctrl+S, and Esc retain their
  existing submit, draft, and cancel meanings, and command payloads remain
  plain field strings. The focused caret and multiline body are rendered in
  the modal, while the priority chooser keeps its non-text control.
