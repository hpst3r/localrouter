#!/usr/bin/env python3
"""Drive the dashboard in headless Chrome via CDP (stdlib only): collect JS
errors, exercise hover/drill-down/zoom/legend, screenshot each state.
Usage: cdp_check.py URL OUTDIR [width height] [dark]

DevTools runs over --remote-debugging-pipe (Chrome reads NUL-terminated JSON
on fd 3 and writes on fd 4), so no TCP debugging endpoint is opened that
another local user could attach to. The Chrome profile is a private mkdtemp()
directory (removed on exit). Chrome's sandbox stays on; set CDP_NO_SANDBOX=1
only where it cannot start (e.g. some containers)."""
import base64, fcntl, json, os, select, shutil, subprocess, sys, tempfile, time

url, out = sys.argv[1], sys.argv[2]
w, h = (int(sys.argv[3]), int(sys.argv[4])) if len(sys.argv) > 4 else (1280, 1700)
dark = len(sys.argv) > 5 and sys.argv[5] == "dark"
os.makedirs(out, exist_ok=True)
prof = tempfile.mkdtemp(prefix="cdp-prof-")
args = ["google-chrome", "--headless=new", "--disable-gpu", "--remote-debugging-pipe",
        f"--user-data-dir={prof}", "--hide-scrollbars", f"--window-size={w},{h}", "about:blank"]
if os.environ.get("CDP_NO_SANDBOX") == "1":
    args.insert(1, "--no-sandbox")
def above_4(fd):
    # Keep the child ends clear of 3/4 so the redirections below cannot clash.
    n = fcntl.fcntl(fd, fcntl.F_DUPFD_CLOEXEC, 10); os.close(fd); return n
to_chrome_r, to_chrome_w = os.pipe()
from_chrome_r, from_chrome_w = os.pipe()
to_chrome_r, from_chrome_w = above_4(to_chrome_r), above_4(from_chrome_w)
# pass_fds keeps the fd numbers, so a shell moves them to the 3/4 Chrome expects.
wrapper = f'exec "$@" 3<&{to_chrome_r} 4>&{from_chrome_w} {to_chrome_r}<&- {from_chrome_w}>&-'
chrome = subprocess.Popen(["/bin/sh", "-c", wrapper, "sh"] + args, pass_fds=(to_chrome_r, from_chrome_w),
                          stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
os.close(to_chrome_r); os.close(from_chrome_w)
try:
    inbuf = b""

    def send(obj):
        data = json.dumps(obj).encode() + b"\0"
        while data:
            data = data[os.write(to_chrome_w, data):]

    def recv_msg(timeout=30):
        global inbuf
        while b"\0" not in inbuf:
            if not select.select([from_chrome_r], [], [], timeout)[0]:
                sys.exit("chrome DevTools pipe timed out")
            chunk = os.read(from_chrome_r, 1 << 16)
            if not chunk:
                sys.exit("chrome DevTools pipe closed")
            inbuf += chunk
        msg, inbuf = inbuf.split(b"\0", 1)
        return json.loads(msg)

    mid = [0]; events = []; session = [None]
    def call(method, **params):
        mid[0] += 1
        req = {"id": mid[0], "method": method, "params": params}
        if session[0]: req["sessionId"] = session[0]
        send(req)
        while True:
            msg = recv_msg()
            if msg.get("id") == mid[0]:
                if "error" in msg: raise RuntimeError(msg["error"])
                return msg.get("result", {})
            if msg.get("sessionId") == session[0]:
                events.append(msg)

    # The pipe speaks to the browser target; attach to the page (flat mode).
    page = None
    for _ in range(50):
        page = next((t for t in call("Target.getTargets")["targetInfos"] if t["type"] == "page"), None)
        if page: break
        time.sleep(0.2)
    if page is None:
        sys.exit("chrome page target did not come up")
    session[0] = call("Target.attachToTarget", targetId=page["targetId"], flatten=True)["sessionId"]

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
    os.close(to_chrome_w); os.close(from_chrome_r)
    chrome.terminate()
    try: chrome.wait(5)
    except Exception: chrome.kill()
    shutil.rmtree(prof, ignore_errors=True)
