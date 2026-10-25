import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import "@copilotkit/react-core/v2/styles.css";
import "../../src/styles/index.scss";
import "./styles/demo.scss";
import { App } from "./App";

const root = document.getElementById("root");
if (root !== null) {
  createRoot(root).render(
    <StrictMode>
      <App />
    </StrictMode>,
  );
}
