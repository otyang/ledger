"""Builds docs/index.html, the GitHub Pages home page, from README.md.

Usage: python3 tools/site/build.py   (or: make site)

It handles the Markdown the README uses: headings, paragraphs, lists (one
level of nesting), tables, fenced code, inline code, bold, italics and links.
Relative links point to the files on GitHub; the README's own Contents list is
replaced by the page's sidebar.
"""
import html, re, sys

REPO = "https://github.com/otyang/ledger/blob/main/"

def slug(text):
    t = re.sub(r"<[^>]+>", "", text)
    t = re.sub(r"[`*]", "", t).strip().lower()
    t = re.sub(r"[^\w\- ]", "", t)
    return t.replace(" ", "-")

def inline(s):
    codes = []
    def keep(m):
        codes.append("<code>" + html.escape(m.group(1)) + "</code>")
        return f"\x00{len(codes)-1}\x00"
    s = re.sub(r"`([^`]+)`", keep, s)
    s = html.escape(s, quote=False)
    def link(m):
        text, href = m.group(1), m.group(2)
        if not href.startswith(("http", "#")):
            href = REPO + href
        ext = ' target="_blank" rel="noopener"' if href.startswith("http") else ""
        return f'<a href="{html.escape(href)}"{ext}>{text}</a>'
    s = re.sub(r"\[([^\]]+)\]\(([^)\s]+)\)", link, s)
    s = re.sub(r"\*\*([^*]+)\*\*", r"<strong>\1</strong>", s)
    s = re.sub(r"(?<![\w*])\*([^*\s][^*]*?)\*(?![\w*])", r"<em>\1</em>", s)
    return re.sub(r"\x00(\d+)\x00", lambda m: codes[int(m.group(1))], s)

LANGS = {"sh": "bash", "go": "go", "json": "json", "http": "http"}

def convert(md):
    lines = md.split("\n")
    out, toc, i = [], [], 0
    def cells(row):
        return [c.strip() for c in row.strip().strip("|").split("|")]
    while i < len(lines):
        line = lines[i]
        if line.startswith("```"):
            lang = line[3:].strip()
            body = []
            i += 1
            while not lines[i].startswith("```"):
                body.append(lines[i]); i += 1
            i += 1
            code = html.escape("\n".join(body))
            label = {"sh": "shell", "http": "http", "json": "json", "go": "go"}.get(lang, lang)
            out.append(f'<figure class="code"><figcaption><span>{label}</span>'
                       f'<button type="button" class="copy">Copy</button></figcaption>'
                       f'<pre><code class="language-{LANGS.get(lang, lang)}">{code}</code></pre></figure>')
            continue
        m = re.match(r"^(#{1,4}) (.+)$", line)
        if m:
            level, text = len(m.group(1)), m.group(2)
            if level == 1:
                # The page header carries the title and the opening sentence.
                i += 1
                while i < len(lines) and not lines[i].strip():
                    i += 1
                while i < len(lines) and lines[i].strip():
                    i += 1
                continue
            sid = slug(text)
            if level == 2:
                toc.append((sid, re.sub(r"<[^>]+>", "", inline(text))))
            out.append(f'<h{level} id="{sid}">{inline(text)}</h{level}>')
            i += 1; continue
        if line.startswith("|"):
            head = cells(line)
            i += 2
            rows = []
            while i < len(lines) and lines[i].startswith("|"):
                rows.append(cells(lines[i])); i += 1
            th = "".join(f"<th>{inline(c)}</th>" for c in head)
            tb = "".join("<tr>" + "".join(f"<td>{inline(c)}</td>" for c in r) + "</tr>" for r in rows)
            out.append(f'<div class="table"><table><thead><tr>{th}</tr></thead><tbody>{tb}</tbody></table></div>')
            continue
        if re.match(r"^- ", line):
            items = []
            while i < len(lines) and re.match(r"^(  )?- ", lines[i]):
                items.append((lines[i].startswith("  "), lines[i].lstrip()[2:])); i += 1
            buf, open_sub = ["<ul>"], False
            for j, (sub, text) in enumerate(items):
                if sub and not open_sub:
                    buf[-1] = buf[-1].removesuffix("</li>") + "<ul>"; open_sub = True
                if not sub and open_sub:
                    buf.append("</ul></li>"); open_sub = False
                buf.append(f"<li>{inline(text)}</li>")
            if open_sub:
                buf.append("</ul></li>")
            buf.append("</ul>")
            out.append("".join(buf))
            continue
        if not line.strip():
            i += 1; continue
        para = []
        while i < len(lines) and lines[i].strip() and not re.match(r"^(```|#|\||- )", lines[i]):
            para.append(lines[i]); i += 1
        out.append(f"<p>{inline(' '.join(para))}</p>")
    return "\n".join(out), toc

import os
root = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
src = sys.argv[1] if len(sys.argv) > 1 else os.path.join(root, "README.md")
template = sys.argv[2] if len(sys.argv) > 2 else os.path.join(root, "tools", "site", "template.html")
dest = sys.argv[3] if len(sys.argv) > 3 else os.path.join(root, "docs", "index.html")
md = open(src).read()
# The page's contents column replaces the README's own Contents list.
md = re.sub(r"\n## Contents\n.*?(?=\n## )", "\n", md, flags=re.S)
body, toc = convert(md)
nav = "\n".join(f'<li><a href="#{sid}">{text}</a></li>' for sid, text in toc)
tpl = open(template).read()
open(dest, "w").write(tpl.replace("{{TOC}}", nav).replace("{{BODY}}", body))
print(f"wrote {os.path.relpath(dest, root)}: {len(toc)} sections")
