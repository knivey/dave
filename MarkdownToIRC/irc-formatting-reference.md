# IRC Formatting Reference

> Source: <https://modern.ircdocs.horse/formatting.html> (archived 2026-04-17)
> Fetched for reference when working on the MarkdownToIRC module.

---

# Introduction

IRC clients today understand a number of special formatting characters. These
characters allow IRC software to send and receive colors and formatting codes
such as bold, italics, underline and others.

Over the years, many clients have attempted to create their own methods of
formatting and there have been variations and extensions of almost every method.
However, the characters and codes described in this document are understood
fairly consistently across clients today.

---

# Characters

Some formatting codes work as a toggle, i.e. with the first instance of the
character, the specified formatting is enabled for the following text. After the
next instance of that character, that formatting is disabled for the following
characters. Formatting codes that work in this way are called 'togglable'.

## Bold

```
Hex:    0x02
Escape: \x02
```

This formatting character works as a toggle. It enables bold text (e.g.
**bold text**).

## Italics

```
Hex:    0x1D
Escape: \x1D
```

This formatting character works as a toggle. It enables italicized text (e.g.
*italicised text*).

## Underline

```
Hex:    0x1F
Escape: \x1F
```

This formatting character works as a toggle. It enables underlined text.

## Strikethrough

```
Hex:    0x1E
Escape: \x1E
```

This formatting character works as a toggle. It enables strikethrough'd text.
This character is a relatively new addition, and was defined by Textual. At
least HexChat, IRCCloud, Konversation, The Lounge and Textual are known to
support it.

## Monospace

```
Hex:    0x11
Escape: \x11
```

This formatting character works as a toggle. It enables monospace'd text. This
character was defined by IRCCloud. A number of other clients including
TheLounge and Textual now support this as well.

## Color

```
Hex:    0x03
Escape: \x03
```

This formatting character sets or resets colors on the following text. Colors
are represented as ASCII digits.

### Forms of Color Codes

In the following list, `<CODE>` represents the color formatting character
`(0x03)`, `<COLOR>` represents one or two ASCII digits (either `0-9` or
`00-99`).

- `<CODE>` — Reset foreground and background colors.
- `<CODE>,` — Reset foreground and background colors and display the `,` character as text.
- `<CODE><COLOR>` — Set the foreground color.
- `<CODE><COLOR>,` — Set the foreground color and display the `,` character as text.
- `<CODE><COLOR>,<COLOR>` — Set the foreground and background color.

The foreground color is the first `<COLOR>`, and the background color is the
second `<COLOR>` (if sent).

If only the foreground color is set, the background color stays the same.

If there are two ASCII digits available where a `<COLOR>` is allowed, then two
characters MUST always be read for it.

### Standard Colors (0–15, 99)

| Code | Color          |
|------|----------------|
| 00   | White          |
| 01   | Black          |
| 02   | Blue           |
| 03   | Green          |
| 04   | Red            |
| 05   | Brown          |
| 06   | Magenta        |
| 07   | Orange         |
| 08   | Yellow         |
| 09   | Light Green    |
| 10   | Cyan           |
| 11   | Light Cyan     |
| 12   | Light Blue     |
| 13   | Pink           |
| 14   | Grey           |
| 15   | Light Grey     |
| 99   | Default FG/BG  |

### Extended Colors (16–98)

Color codes 16–98 map to specific RGB values:

| Code | RGB (hex) | Code | RGB (hex) | Code | RGB (hex) | Code | RGB (hex) |
|------|-----------|------|-----------|------|-----------|------|-----------|
| 16   | 470000    | 32   | 007400    | 48   | 0000b5    | 64   | ff5959    |
| 17   | 472100    | 33   | 007449    | 49   | 7500b5    | 65   | ffb459    |
| 18   | 474700    | 34   | 007474    | 50   | b500b5    | 66   | ffff71    |
| 19   | 324700    | 35   | 004074    | 51   | b5006b    | 67   | cfff60    |
| 20   | 004700    | 36   | 000074    | 52   | ff0000    | 68   | 6fff6f    |
| 21   | 00472c    | 37   | 4b0074    | 53   | ff8c00    | 69   | 65ffc9    |
| 22   | 004747    | 38   | 740047    | 54   | ffff00    | 70   | 6dffff    |
| 23   | 002747    | 39   | 740045    | 55   | b2ff00    | 71   | 59b4ff    |
| 24   | 000047    | 40   | b50000    | 56   | 00ff00    | 72   | 5959ff    |
| 25   | 2e0047    | 41   | b56300    | 57   | 00ffa0    | 73   | c459ff    |
| 26   | 470047    | 42   | b5b500    | 58   | 00ffff    | 74   | ff66ff    |
| 27   | 47002a    | 43   | 7db500    | 59   | 008cff    | 75   | ff59bc    |
| 28   | 740000    | 44   | 00b500    | 60   | 0000ff    | 76   | ff9c9c    |
| 29   | 743a00    | 45   | 00b571    | 61   | a500ff    | 77   | ffd39c    |
| 30   | 747400    | 46   | 00b5b5    | 62   | ff00ff    | 78   | ffff9c    |
| 31   | 517400    | 47   | 0063b5    | 63   | ff0098    | 79   | e2ff9c    |

| Code | RGB (hex) | Code | RGB (hex) |
|------|-----------|------|-----------|
| 80   | 9cff9c    | 90   | 282828    |
| 81   | 9cffdb    | 91   | 363636    |
| 82   | 9cffff    | 92   | 4d4d4d    |
| 83   | 9cd3ff    | 93   | 656565    |
| 84   | 9c9cff    | 94   | 818181    |
| 85   | dc9cff    | 95   | 9f9f9f    |
| 86   | ff9cff    | 96   | bcbcbc    |
| 87   | ff94d3    | 97   | e2e2e2    |
| 88   | 000000    | 98   | ffffff    |
| 89   | 131313    |      |           |

### ANSI Terminal Equivalents for Extended Colors

| IRC  | ANSI | IRC  | ANSI | IRC  | ANSI | IRC  | ANSI |
|------|------|------|------|------|------|------|------|
| 16   | 52   | 33   | 35   | 50   | 127  | 67   | 191  |
| 17   | 94   | 34   | 30   | 51   | 161  | 68   | 83   |
| 18   | 100  | 35   | 25   | 52   | 196  | 69   | 122  |
| 19   | 58   | 36   | 18   | 53   | 208  | 70   | 87   |
| 20   | 22   | 37   | 91   | 54   | 226  | 71   | 111  |
| 21   | 29   | 38   | 90   | 55   | 154  | 72   | 63   |
| 22   | 23   | 39   | 125  | 56   | 46   | 73   | 177  |
| 23   | 24   | 40   | 124  | 57   | 86   | 74   | 207  |
| 24   | 17   | 41   | 166  | 58   | 51   | 75   | 205  |
| 25   | 54   | 42   | 184  | 59   | 75   | 76   | 217  |
| 26   | 53   | 43   | 106  | 60   | 21   | 77   | 223  |
| 27   | 89   | 44   | 34   | 61   | 171  | 78   | 229  |
| 28   | 88   | 45   | 49   | 62   | 201  | 79   | 193  |
| 29   | 130  | 46   | 37   | 63   | 198  | 80   | 157  |
| 30   | 142  | 47   | 33   | 64   | 203  | 81   | 158  |
| 31   | 64   | 48   | 19   | 65   | 215  | 82   | 159  |
| 32   | 28   | 49   | 129  | 66   | 227  | 83   | 153  |

| IRC  | ANSI | IRC  | ANSI |
|------|------|------|------|
| 84   | 147  | 92   | 239  |
| 85   | 183  | 93   | 241  |
| 86   | 219  | 94   | 244  |
| 87   | 212  | 95   | 247  |
| 88   | 16   | 96   | 250  |
| 89   | 233  | 97   | 254  |
| 90   | 235  | 98   | 231  |
| 91   | 237  |      |      |

### Mistaken Eating of Text

When sending color codes `0-9`, clients may use either the one-digit (`3`) or
two-digit (`03`) versions. However, since two digits are always used if
available, if the text following the color code starts with a digit, the last
`<COLOR>` MUST use the two-digit version to be displayed correctly. This ensures
that the first character of the text does not get interpreted as part of the
formatting code.

If the text immediately following a code setting a foreground color consists of
something like `",13"`, it will get interpreted as setting the background rather
than text. In this example, clients can put the color code either after the
comma character or before the character in front of the comma character to avoid
this. They can also put a different formatting code after the comma to ensure
that the number does not get interpreted as part of the color code (for instance,
two bold characters in a row, which will cancel each other out as they are
toggles).

### Spoilers

If the background and foreground colors are the same for a section of text, on
'hovering over' or selecting this text these colours should be replaced with
readable alternatives.

## Hex Color

```
Hex:    0x04
Escape: \x04
```

Some clients support an alternate form of conveying colours using hex codes.
Following this character are six hex digits representing the Red, Green and Blue
values of the colour to display (e.g. `FF0000` means bright red).

Keeps the same rules as the Color code forms, but `<COLOR>` represents a
six-digit hex value as `RRGGBB`.

This method of formatting is not as widely-supported as the numeric colors
above, but clients are fine to parse them without any negative effects.

## Reverse Color

```
Hex:    0x16
Escape: \x16
```

This formatting character works as a toggle. When reverse color is enabled, the
foreground and background text colors are reversed. For instance, if you enable
reverse color and then send the line "C3,13Test!", you will end up with pink
foreground text and green background text while the reverse color is in effect.

This code isn't super well-supported, and mIRC seems to always treat it as
applying the reverse of the default foreground and background characters, rather
than the current fore/background as set by prior mIRC color codes.

## Reset

```
Hex:    0x0F
Escape: \x0F
```

This formatting character resets all formatting. It removes the bold, italics,
and underline formatting, and sets the foreground and background colors back to
the default for the client display. The text following this character will use
or display no formatting, until other formatting characters are encountered.

---

# Quick Reference — All Formatting Characters

| Name          | Hex    | Escaped  | Type    |
|---------------|--------|----------|---------|
| Bold          | `0x02` | `\x02`   | Toggle  |
| Italics       | `0x1D` | `\x1D`   | Toggle  |
| Underline     | `0x1F` | `\x1F`   | Toggle  |
| Strikethrough | `0x1E` | `\x1E`   | Toggle  |
| Monospace     | `0x11` | `\x11`   | Toggle  |
| Color         | `0x03` | `\x03`   | Set/Reset |
| Hex Color     | `0x04` | `\x04`   | Set/Reset |
| Reverse Color | `0x16` | `\x16`   | Toggle  |
| Reset         | `0x0F` | `\x0F`   | Reset   |

---

# Formatting Uses

Formatting is allowed and commonly used in:
- `PRIVMSG`, `NOTICE`, `TOPIC`, `AWAY` messages
- `USER` realnames (not usernames)
- Message of the Day (MOTD)
- vhosts (vanity hostnames) on some networks

Formatting MUST NOT be allowed in nicknames, usernames, or channel names.

---

# Examples

In these examples, `C` = color (0x03), `B` = bold (0x02), `I` = italics (0x1D),
`O` = reset (0x0F).

- `I love C3IRC! CIt is the C7best protocol ever!`
  → "I love" (normal) "IRC!" (green) " It is the " (normal) "best protocol ever!" (orange)

- `This is a IC13,9cool Cmessage`
  → "This is a " (italic) "cool" (pink on light green) " message" (normal)

- `IRC Bis C4,12so CgreatO!`
  → "IRC" (bold) " is " (red on light blue) "so " (default) "great" (reset) "!"

---

*Original document by Daniel Oaks. Canonical version: <https://modern.ircdocs.horse/formatting.html>*
