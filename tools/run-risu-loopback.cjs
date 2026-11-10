// Runs the RisuAI node server bound to 127.0.0.1 only, without editing RisuAI.
// usage: node run-risu-loopback.cjs <risuai-dir> [port]
const net = require("net");
const path = require("path");

const dir = path.resolve(process.argv[2] || ".");
process.env.PORT = process.argv[3] || process.env.PORT || "28192";

const origListen = net.Server.prototype.listen;
net.Server.prototype.listen = function (...args) {
  // listen(port[, cb]) or listen({port}) → force loopback host
  if (typeof args[0] === "number" || (typeof args[0] === "string" && /^\d+$/.test(args[0]))) {
    const port = Number(args[0]);
    const rest = args.slice(1).filter((a) => typeof a === "function");
    return origListen.call(this, port, "127.0.0.1", ...rest);
  }
  if (args[0] && typeof args[0] === "object" && args[0].port !== undefined) {
    args[0] = { ...args[0], host: "127.0.0.1" };
  }
  return origListen.apply(this, args);
};

process.chdir(dir);
require(path.join(dir, "server", "node", "server.cjs"));
