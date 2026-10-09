#!/usr/bin/env python3
"""Drive the dashboard in headless Chrome via CDP (stdlib only): collect JS
errors, exercise hover/drill-down/zoom/legend, screenshot each state.
Usage: cdp_check.py URL OUTDIR [width height] [dark]

The Chrome profile is a private mkdtemp() directory (removed on exit) and the
DevTools port is an OS-assigned ephemeral port on 127.0.0.1, read back from the
profile's DevToolsActivePort file. Chrome's sandbox stays on; set
CDP_NO_SANDBOX=1 only where it cannot start (e.g. some containers)."""
import base64, json, os, shutil, socket, subprocess, sys, tempfile, time, urllib.request, struct

url, out = sys.argv[1], sys.argv[2]
w, h = (int(sys.argv[3]), int(sys.argv[4])) if len(sys.argv) > 4 else (1280, 1700)
dark = len(sys.argv) > 5 and sys.argv[5] == "dark"
os.makedirs(out, exist_ok=True)
prof = tempfile.mkdtemp(prefix="cdp-prof-")
args = ["google-chrome", "--headless=new", "--disable-gpu",
        "--remote-debugging-address=127.0.0.1", "--remote-debugging-port=0",
        f"--user-data-dir={prof}", "--hide-scrollbars", f"--window-size={w},{h}", "about:blank"]
if os.environ.get("CDP_NO_SANDBOX") == "1":
    args.insert(1, "--no-sandbox")
chrome = subprocess.Popen(args, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
try:
    ws_url = None
    for _ in range(50):
        try:
            with open(os.path.join(prof, "DevToolsActivePort")) as f:
                port = int(f.readline().strip())
            tabs = json.load(urllib.request.urlopen(f"http://127.0.0.1:{port}/json"))
            ws_url = [t for t in tabs if t["type"] == "page"][0]["webSocketDebuggerUrl"]
            break
        except Exception:
            time.sleep(0.2)
    if ws_url is None:
        sys.exit("chrome DevTools endpoint did not come up")
    # minimal websocket client
    host, rest = ws_url[len("ws://"):].split("/", 1)
    hh, pp = host.split(":")
    s = socket.create_connection((hh, int(pp)))
    key = base64.b64encode(os.urandom(16)).decode()
    s.send(f"GET /{rest} HTTP/1.1\r\nHost: {host}\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: {key}\r\nSec-WebSocket-Version: 13\r\n\r\n".encode())
    buf = b""
    while b"\r\n\r\n" not in buf:
        buf += s.recv(4096)
    buf = buf.split(b"\r\n\r\n", 1)[1]

    def send(obj):
        data = json.dumps(obj).encode()
        hdr = bytearray([0x81])
        n = len(data)
        if n < 126: hdr.append(0x80 | n)
        elif n < 65536: hdr += bytes([0x80 | 126]) + struct.pack(">H", n)
        else: hdr += bytes([0x80 | 127]) + struct.pack(">Q", n)
        mask = os.urandom(4); hdr += mask
        s.sendall(bytes(hdr) + bytes(b ^ mask[i % 4] for i, b in enumerate(data)))

    def recv_frame():
        nonlocal_buf = recv_frame.buf
        def need(k):
            while len(recv_frame.buf) < k:
                recv_frame.buf += s.recv(1 << 16)
        need(2)
        b0, b1 = recv_frame.buf[0], recv_frame.buf[1]
        n = b1 & 0x7f; off = 2
        if n == 126: need(4); n = struct.unpack(">H", recv_frame.buf[2:4])[0]; off = 4
        elif n == 127: need(10); n = struct.unpack(">Q", recv_frame.buf[2:10])[0]; off = 10
        need(off + n)
        payload = recv_frame.buf[off:off + n]; recv_frame.buf = recv_frame.buf[off + n:]
        return payload
    recv_frame.buf = buf

    mid = [0]; events = []
    def call(method, **params):
        mid[0] += 1; send({"id": mid[0], "method": method, "params": params})
        while True:
            msg = json.loads(recv_frame())
            if msg.get("id") == mid[0]:
                if "error" in msg: raise RuntimeError(msg["error"])
                return msg.get("result", {})
            events.append(msg)

    def ev(expr):
        r = call("Runtime.evaluate", expression=expr, returnByValue=True, awaitPromise=True)
        if r.get("exceptionDetails"): return {"__exception__": r["exceptionDetails"].get("text"), "detail": str(r["exceptionDetails"].get("exception", {}).get("description", ""))[:400]}
        return r["result"].get("value")

    def shot(name):
        r = call("Page.captureScreenshot", format="png", captureBeyondViewport=True)
        p = os.path.join(out, name + ".png"); open(p, "wb").write(base64.b64decode(r["data"])); print("SHOT", p)

    call("Runtime.enable"); call("Page.enable"); call("Log.enable")
    if dark:
        call("Emulation.setEmulatedMedia", features=[{"name": "prefers-color-scheme", "value": "dark"}])
    call("Emulation.setDeviceMetricsOverride", width=w, height=h, deviceScaleFactor=1, mobile=False)
    call("Page.navigate", url=url); time.sleep(2.5)

    def errors():
        out_ = []
        for m in events:
            if m.get("method") == "Runtime.exceptionThrown":
                out_.append(m["params"]["exceptionDetails"].get("exception", {}).get("description", m["params"]["exceptionDetails"].get("text"))[:300])
            if m.get("method") == "Runtime.consoleAPICalled" and m["params"]["type"] in ("error", "warning"):
                out_.append("console." + m["params"]["type"] + ": " + " ".join(str(a.get("value", a.get("description", ""))) for a in m["params"]["args"])[:300])
            if m.get("method") == "Log.entryAdded" and m["params"]["entry"]["level"] in ("error",):
                out_.append("log: " + m["params"]["entry"]["text"][:300])
        return out_

    shot("01-initial")
    print("STATE0", json.dumps(ev("""(()=>({hash:location.hash,
      segs:document.querySelectorAll('svg [data-key]').length,
      title:document.title, crumbs:[...document.querySelectorAll('[data-crumb], .crumb, .crumbs button')].map(e=>e.textContent.trim()) }))()""")))
    # hover a segment
    print("HOVER", json.dumps(ev("""(()=>{const el=document.querySelector('svg [data-key]'); if(!el) return 'no segment';
      const r=el.getBoundingClientRect(); ['pointerover','pointerenter','mouseover','mouseenter','pointermove','mousemove'].forEach(t=>el.dispatchEvent(new MouseEvent(t,{bubbles:true,clientX:r.x+r.width/2,clientY:r.y+r.height/2})));
      const tip=[...document.querySelectorAll('[role=tooltip], .tooltip, #tooltip')].find(t=>t.offsetParent!==null||getComputedStyle(t).display!=='none');
      return tip? tip.textContent.trim().slice(0,200) : 'no tooltip visible';})()""")))
    shot("02-hover")
    # leave the chart like a real pointer would (redraws are deferred while hovering)
    ev("""(()=>{const c=document.getElementById('chart')||document.querySelector('svg'); ['pointerleave','mouseleave','pointerout','mouseout'].forEach(t=>c.dispatchEvent(new MouseEvent(t,{bubbles:t.endsWith('out')}))); return 1})()""")
    time.sleep(0.3)
    # drill down: click a segment
    print("CLICK-SEG", json.dumps(ev("""(()=>{const el=document.querySelector('svg [data-key]'); if(!el) return 'none'; const k=el.getAttribute('data-key'); el.dispatchEvent(new MouseEvent('click',{bubbles:true})); return k;})()""")))
    time.sleep(1.5)
    print("STATE1", json.dumps(ev("""(()=>({hash:location.hash, segs:document.querySelectorAll('svg [data-key]').length,
      keys:[...new Set([...document.querySelectorAll('svg [data-key]')].map(e=>e.getAttribute('data-key')))],
      groupSel:(document.querySelector('select')||{}).value, crumbs:[...document.querySelectorAll('nav button, .crumbs button, [data-crumb]')].map(e=>e.textContent.trim()).slice(0,10)}))()""")))
    shot("03-drilled")
    # zoom: click an x-axis label/bucket
    print("ZOOM", json.dumps(ev("""(()=>{const el=document.querySelector('svg [data-bucket], svg .xlabel, svg text[data-i]'); if(!el) return 'no bucket target'; el.dispatchEvent(new MouseEvent('click',{bubbles:true})); return el.textContent||el.getAttribute('data-bucket');})()""")))
    time.sleep(1.5)
    print("STATE2", json.dumps(ev("(()=>({hash:location.hash, segs:document.querySelectorAll('svg [data-key]').length, bars:document.querySelectorAll('svg rect.hit').length}))()")))
    shot("04-zoomed")
    # back
    call("Runtime.evaluate", expression="history.back()"); time.sleep(1.5)
    print("BACK", json.dumps(ev("(()=>location.hash)()")))
    # table sort
    print("SORT", json.dumps(ev("""(()=>{const th=[...document.querySelectorAll('th')].find(t=>/Output/i.test(t.textContent)); if(!th) return 'no th'; (th.querySelector('button')||th).click(); return [...document.querySelectorAll('tbody tr td:first-child')].map(t=>t.textContent.trim()).slice(0,5);})()""")))
    time.sleep(0.5)
    print("ERRORS", json.dumps(errors()))
finally:
    chrome.terminate()
    try: chrome.wait(5)
    except Exception: chrome.kill()
    shutil.rmtree(prof, ignore_errors=True)
