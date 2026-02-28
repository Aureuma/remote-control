const fs = require("node:fs");
const net = require("node:net");
const os = require("node:os");
const path = require("node:path");
const { spawn, spawnSync } = require("node:child_process");
const { test, expect } = require("@playwright/test");

const repoRoot = path.resolve(__dirname, "..", "..");
const buildDir = path.join(repoRoot, ".tmp", "playwright");
const binaryPath = path.join(buildDir, "remote-control-e2e");
let binaryReady = false;

const stubCSSURL = "https://cdn.jsdelivr.net/npm/xterm@5.5.0/css/xterm.min.css";
const stubXtermURL = "https://cdn.jsdelivr.net/npm/xterm@5.5.0/lib/xterm.min.js";
const stubFitURL = "https://cdn.jsdelivr.net/npm/xterm-addon-fit@0.10.0/lib/xterm-addon-fit.min.js";

const xtermStubJS = `
(() => {
  class StubTerminal {
    constructor() {
      this.cols = 80;
      this.rows = 24;
      this._onData = [];
      this._onResize = [];
      window.__stubTerminal = this;
    }
    loadAddon(addon) { this._addon = addon; }
    open(element) {
      this._element = element;
      if (this._element) this._element.setAttribute("data-terminal-ready", "true");
    }
    onData(cb) { this._onData.push(cb); }
    onResize(cb) { this._onResize.push(cb); }
    write(data, cb) {
      const text = typeof data === "string" ? data : new TextDecoder().decode(data);
      if (this._element) this._element.textContent += text;
      if (!window.__terminalOutput) window.__terminalOutput = [];
      window.__terminalOutput.push(text);
      if (typeof cb === "function") cb();
    }
    writeln(data) { this.write(String(data) + "\\n"); }
    emitData(data) {
      for (const cb of this._onData) cb(data);
    }
  }
  window.Terminal = StubTerminal;
  window.__emitTerminalData = (data) => {
    if (window.__stubTerminal) window.__stubTerminal.emitData(data);
  };
})();
`;

const fitStubJS = `
window.FitAddon = {
  FitAddon: class FitAddon {
    fit() {}
  }
};
`;

function ensureBinaryBuilt() {
  if (binaryReady) return;
  fs.mkdirSync(buildDir, { recursive: true });
  const result = spawnSync("go", ["build", "-o", binaryPath, "./cmd/remote-control"], {
    cwd: repoRoot,
    encoding: "utf8",
  });
  if (result.status !== 0) {
    throw new Error(`go build failed:\\n${result.stdout}\\n${result.stderr}`);
  }
  binaryReady = true;
}

function allocatePort() {
  return new Promise((resolve, reject) => {
    const server = net.createServer();
    server.listen(0, "127.0.0.1", () => {
      const address = server.address();
      if (!address || typeof address === "string") {
        server.close(() => reject(new Error("failed to allocate local port")));
        return;
      }
      const { port } = address;
      server.close((err) => {
        if (err) reject(err);
        else resolve(port);
      });
    });
    server.on("error", reject);
  });
}

function waitForExit(proc, timeoutMs) {
  return new Promise((resolve) => {
    let settled = false;
    const timer = setTimeout(() => {
      if (settled) return;
      settled = true;
      resolve({ timeout: true });
    }, timeoutMs);
    proc.once("exit", (code, signal) => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      resolve({ timeout: false, code, signal });
    });
  });
}

async function startSession({ readwrite }) {
  ensureBinaryBuilt();
  const homeDir = fs.mkdtempSync(path.join(os.tmpdir(), "rc-browser-e2e-"));
  const id = `browser-${Date.now()}-${Math.floor(Math.random() * 100000)}`;
  const port = await allocatePort();
  const command = "cat";
  const args = [
    "start",
    "--cmd",
    command,
    "--bind",
    "127.0.0.1",
    "--port",
    String(port),
    "--id",
    id,
    "--no-tunnel",
    "--no-caffeinate",
  ];
  if (readwrite) args.push("--readwrite");

  const proc = spawn(binaryPath, args, {
    cwd: repoRoot,
    env: { ...process.env, SI_REMOTE_CONTROL_HOME: homeDir },
    stdio: ["ignore", "pipe", "pipe"],
  });

  let logs = "";
  let resolved = false;
  let shareURL = "";

  const collect = (chunk) => {
    const text = chunk.toString();
    logs += text;
    const match = text.match(/Share URL:\s*(\S+)/);
    if (match && match[1]) {
      shareURL = match[1].trim();
      resolved = true;
    }
  };
  proc.stdout.on("data", collect);
  proc.stderr.on("data", collect);

  const started = await new Promise((resolve, reject) => {
    const timeout = setTimeout(() => {
      reject(new Error(`timed out waiting for share URL\\n${logs}`));
    }, 20000);
    const poll = setInterval(() => {
      if (resolved) {
        clearTimeout(timeout);
        clearInterval(poll);
        resolve(true);
      }
    }, 50);
    proc.once("exit", (code, signal) => {
      clearTimeout(timeout);
      clearInterval(poll);
      reject(new Error(`remote-control exited early (code=${code}, signal=${signal})\\n${logs}`));
    });
  });
  if (!started) throw new Error("failed to start session");

  return {
    proc,
    id,
    homeDir,
    shareURL,
    logs: () => logs,
  };
}

async function stopSession(session) {
  if (!session) return;
  const stopResult = spawnSync(binaryPath, ["stop", "--id", session.id], {
    cwd: repoRoot,
    env: { ...process.env, SI_REMOTE_CONTROL_HOME: session.homeDir },
    encoding: "utf8",
  });
  if (stopResult.status !== 0) {
    session.proc.kill("SIGTERM");
  }
  const exited = await waitForExit(session.proc, 7000);
  if (exited.timeout) {
    session.proc.kill("SIGKILL");
    await waitForExit(session.proc, 3000);
  }
  fs.rmSync(session.homeDir, { recursive: true, force: true });
}

async function installCDNStubs(page) {
  await page.route(stubCSSURL, (route) =>
    route.fulfill({
      status: 200,
      contentType: "text/css",
      body: "",
    }),
  );
  await page.route(stubXtermURL, (route) =>
    route.fulfill({
      status: 200,
      contentType: "application/javascript",
      body: xtermStubJS,
    }),
  );
  await page.route(stubFitURL, (route) =>
    route.fulfill({
      status: 200,
      contentType: "application/javascript",
      body: fitStubJS,
    }),
  );
}

test.describe("browser remote control", () => {
  let session;

  test.afterEach(async () => {
    await stopSession(session);
    session = undefined;
  });

  test("connects and streams output in readwrite mode", async ({ page }) => {
    session = await startSession({ readwrite: true });
    await installCDNStubs(page);

    await page.goto(session.shareURL);
    await expect(page.getByText("SI Remote Control")).toBeVisible();
    await expect(page.locator("#status")).toHaveText(/Live session/i);

    await page.evaluate(() => window.__emitTerminalData("hello-from-browser\\n"));
    await expect
      .poll(async () => page.locator("#terminal").textContent())
      .toContain("hello-from-browser");
  });

  test("shows read-only state and blocks browser input writes", async ({ page }) => {
    session = await startSession({ readwrite: false });
    await installCDNStubs(page);

    await page.goto(session.shareURL);
    await expect(page.locator("#status")).toHaveText(/Read-only/i);

    await page.evaluate(() => window.__emitTerminalData("should-not-write\\n"));
    await page.waitForTimeout(600);
    const output = await page.locator("#terminal").textContent();
    expect(output).not.toContain("should-not-write");
  });

  test("rejects second browser connection when max clients is 1", async ({ browser, page }) => {
    session = await startSession({ readwrite: true });
    await installCDNStubs(page);
    await page.goto(session.shareURL);
    await expect(page.locator("#status")).toHaveText(/Live session/i);

    const page2 = await browser.newPage();
    await installCDNStubs(page2);
    await page2.goto(session.shareURL);
    await expect
      .poll(async () => page2.locator("#terminal").textContent())
      .toContain("another client is already connected");
    await page2.close();
  });
});
