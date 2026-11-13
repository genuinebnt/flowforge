#!/usr/bin/env python3
"""Render docs/flowforge-spec.html to docs/spec.txt (stable-line-number text form).

Usage: python3 scripts/spec2txt.py docs/flowforge-spec.html > docs/spec.txt

Rules: <head> contributes only its <title>; <style>, <script> and <svg> are
dropped; the <nav> table of contents is one block with one link per line,
split into parts at each <strong>: the first part's lines are printed where
the nav is, and part k (k >= 2) is printed right before the element that
carries data-toc-part="k" (the start of that part), so adding a part never
moves the line numbers of an earlier one;
headings become Markdown '#' lines; <li> becomes '- '; each table row becomes
' | cell | cell'; <pre> keeps its text verbatim; every other block element
(p, div, header, footer, ...) ends a paragraph. Blocks are separated by one
blank line. Inline markup is dropped and whitespace in non-pre text collapses.
"""
import html.parser
import re
import sys

BLOCK = {"p", "div", "header", "footer", "main", "nav", "table", "ul", "ol",
         "li", "tr", "h1", "h2", "h3", "h4", "pre", "body", "section"}
SKIP = {"style", "script", "svg"}


class R(html.parser.HTMLParser):
    def __init__(self):
        super().__init__(convert_charrefs=True)
        self.blocks = []
        self.buf = []
        self.prefix = ""
        self.skip = 0
        self.pre = 0
        self.in_head = False
        self.in_title = False
        self.title = ""
        self.nav = 0
        self.navlines = []
        self.navparts = []
        self.cells = None
        self.cell = None

    def flush(self):
        if self.pre:
            return
        t = re.sub(r"\s+", " ", "".join(self.buf)).strip()
        self.buf = []
        if t:
            self.blocks.append(self.prefix + t)
        self.prefix = ""

    def handle_starttag(self, tag, attrs):
        if self.skip:
            if tag in SKIP:
                self.skip += 1
            return
        if tag in SKIP:
            self.skip = 1
            return
        if tag == "head":
            self.in_head = True
            return
        if tag == "title":
            self.in_title = True
            return
        if self.in_head:
            return
        if tag == "nav":
            self.flush()
            self.nav = 1
            self.navlines = []
            self.buf = []
            return
        if self.nav:
            if tag == "strong":
                self.navparts.append([])
            if tag in ("a", "strong"):
                self.buf = []
            return
        part = dict(attrs).get("data-toc-part")
        if part is not None:
            self.flush()
            k = int(part)
            if k - 1 < len(self.navparts):
                self.blocks.append("\n".join(self.navparts[k - 1]))
        if tag == "tr":
            self.flush()
            self.cells = []
            return
        if tag in ("td", "th") and self.cells is not None:
            self.cell = []
            return
        if tag == "pre":
            self.flush()
            self.pre = 1
            self.buf = []
            return
        if tag in BLOCK:
            self.flush()
            if tag == "li":
                self.prefix = "- "
            elif tag in ("h1", "h2", "h3", "h4"):
                self.prefix = "#" * int(tag[1]) + " "

    def handle_endtag(self, tag):
        if self.skip:
            if tag in SKIP:
                self.skip -= 1
            return
        if tag == "head":
            self.in_head = False
            return
        if tag == "title":
            self.in_title = False
            return
        if self.in_head:
            return
        if self.nav:
            if tag in ("a", "strong"):
                self.navparts[-1].append(re.sub(r"\s+", " ", "".join(self.buf)).strip())
                self.buf = []
            elif tag == "nav":
                self.nav = 0
                self.blocks.append("\n".join(self.navparts[0]))
                self.buf = []
            return
        if tag in ("td", "th") and self.cell is not None:
            self.cells.append(re.sub(r"\s+", " ", "".join(self.cell)))
            self.cell = None
            return
        if tag == "tr" and self.cells is not None:
            self.blocks.append("".join(" | " + c for c in self.cells))
            self.cells = None
            return
        if tag == "pre":
            self.pre = 0
            self.blocks.append("".join(self.buf).rstrip("\n"))
            self.buf = []
            return
        if tag in BLOCK:
            self.flush()

    def handle_data(self, data):
        if self.skip:
            return
        if self.in_title:
            self.title += data
            return
        if self.in_head:
            return
        if self.cell is not None:
            self.cell.append(data)
            return
        self.buf.append(data)


def main():
    if len(sys.argv) != 2:
        sys.exit("usage: spec2txt.py <spec.html>  (writes the text form to stdout)")
    src = open(sys.argv[1], encoding="utf-8").read()
    r = R()
    r.feed(src)
    r.close()
    r.flush()
    out = "\n\n" + r.title.strip() + "\n\n" + "\n\n".join(r.blocks) + "\n\n"
    sys.stdout.write(out)


if __name__ == "__main__":
    main()
