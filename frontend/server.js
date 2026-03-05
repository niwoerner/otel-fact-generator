const express = require("express");
const http = require("http");
const path = require("path");

const app = express();
const CONTEXT_FETCHER_URL = process.env.CONTEXT_FETCHER_URL || "http://context-fetcher:8080";

app.use(express.static(path.join(__dirname, "public")));

app.get("/api/fact", (req, res) => {
  const url = `${CONTEXT_FETCHER_URL}/fact`;

  http
    .get(url, (upstream) => {
      res.writeHead(upstream.statusCode, {
        "Content-Type": "application/json",
      });
      upstream.pipe(res);
    })
    .on("error", (err) => {
      console.error("Proxy error:", err.message);
      res.status(502).json({ error: "Failed to reach context-fetcher" });
    });
});

const PORT = 3000;
app.listen(PORT, () => {
  console.log(`Frontend listening on :${PORT}`);
});
