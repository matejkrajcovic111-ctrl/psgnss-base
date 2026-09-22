#!/usr/bin/env python3
"""Generate internal/receiver/keydb.json, the u-blox configuration key catalogue.

Sources, in priority order (the first source to name a key wins):

  1. u-blox interface descriptions, converted with `pdftotext -layout`.
     Pass the one matching the deployed firmware first (HPG 2.00 for the
     production ZED-X20P), then later X20 releases for keys added since.
  2. pyubx2's ubxtypes_configdb.py (BSD-3-Clause), itself transcribed from
     u-blox interface specifications. It supplies names for keys the X20
     documents do not list; it has no descriptions, units or scales.

Keys that no source names are left out. The UI shows them as undocumented
rather than inventing a name from neighbouring key IDs.

  gen.py --pyubx2 ubxtypes_configdb.py HPG200.txt HPG202.txt HPG210.txt > keydb.json
"""
import argparse, json, re, sys

ROW = re.compile(r'^(CFG-[A-Z0-9_-]+)\s+(0x[0-9a-f]{8})\s+([A-Z][A-Z0-9]*)\s+(\S+)\s+(\S+)\s*(.*)$')
KEYLINE = re.compile(r'^(CFG-[A-Z0-9_-]+)\s+(0x[0-9a-f]{8})\s+([A-Z][A-Z0-9]*)\s')
CONST = re.compile(r'^([A-Z][A-Z0-9_]*)\s{2,}(-?\d+|0x[0-9a-fA-F]+)\s{2,}(.*)$')
CAPTION = re.compile(r'^Table \d+: Constants for (CFG-[A-Z0-9_-]+)')
PORT_KEY = re.compile(r'^CFG-MSGOUT-(UBX|NMEA|RTCM|PUBX)_(.+)_(I2C|UART1|UART2|USB|SPI)$')


def parse_pdf_text(path):
    lines = open(path, encoding='utf-8').read().split('\n')
    items, full, enums, pending = {}, {}, {}, []
    for i, line in enumerate(lines):
        m = KEYLINE.match(line)
        # Defaults tables print the full name on one line; item tables wrap it.
        if m and m.group(1)[-1] not in '_-':
            full[int(m.group(2), 16)] = m.group(1)
        m = ROW.match(line)
        if m:
            name, kid, typ, scale, unit, desc = m.groups()
            nxt = lines[i + 1] if i + 1 < len(lines) else ''
            rest = ''
            if nxt.strip() and not KEYLINE.match(nxt) and not nxt.startswith('   See'):
                head = nxt.split(None, 1)
                if not nxt.startswith(' ') and name[-1] in '_-' and re.fullmatch(r'[A-Z0-9_]+', head[0]):
                    name += head[0]
                    rest = head[1].strip() if len(head) > 1 else ''
                elif nxt.startswith(' '):
                    rest = nxt.strip()
            if rest and desc and not rest.startswith('Table') and len(rest) < 80:
                desc = desc + ' ' + rest
            items.setdefault(int(kid, 16), {
                'name': name, 'type': typ,
                'scale': '' if scale == '-' else scale,
                'unit': '' if unit == '-' else unit,
                'desc': desc.strip()})
            continue
        if line.startswith('Constant ') and 'Value' in line:
            pending = []
            continue
        m = CONST.match(line)
        if m:
            pending.append({'name': m.group(1), 'value': int(m.group(2), 0), 'desc': m.group(3).strip()})
            continue
        m = CAPTION.match(line)
        if m:
            enums.setdefault(m.group(1), []).extend(pending)
            pending = []
    for k, name in full.items():
        if k in items:
            items[k]['name'] = name
    return items, enums


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('--pyubx2', required=True)
    ap.add_argument('docs', nargs='+')
    a = ap.parse_args()
    items, enums = {}, {}
    for doc in a.docs:
        it, en = parse_pdf_text(doc)
        for k, v in it.items():
            v['source'] = 'u-blox'
            items.setdefault(k, v)
        for n, c in en.items():
            enums.setdefault(n, c)
    src = open(a.pyubx2, encoding='utf-8').read()
    for n, kid, typ in re.findall(r'"(CFG_[A-Z0-9_]+)":\s*\((0x[0-9A-Fa-f]+),\s*([A-Z0-9]+)\)', src):
        k = int(kid, 16)
        if k in items:
            continue
        name = 'CFG-' + n[4:].replace('_', '-', 1)
        desc = ''
        m = PORT_KEY.match(name)
        if m:
            msg = m.group(1) + '-' + m.group(2).replace('_', '-', 1) if m.group(1) == 'UBX' else m.group(1) + '-' + m.group(2)
            desc = 'Output rate of the %s message on port %s' % (msg, m.group(3))
        items[k] = {'name': name, 'type': typ, 'scale': '', 'unit': '', 'desc': desc, 'source': 'pyubx2'}
    for v in items.values():
        e = enums.get(v['name'])
        if e:
            v['enum'] = e
    out = {'%08X' % k: items[k] for k in sorted(items)}
    json.dump(out, sys.stdout, indent=None, separators=(',', ':'), sort_keys=True)
    sys.stdout.write('\n')
    print('%d keys, %d enums' % (len(out), sum(1 for v in out.values() if 'enum' in v)), file=sys.stderr)


if __name__ == '__main__':
    main()
