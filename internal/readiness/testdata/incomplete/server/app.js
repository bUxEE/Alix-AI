const express = require("express");
const app = express();
app.get("/plants", (req, res) => res.json([]));
module.exports = app;
