import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import Portal from "./Portal";
import { ErrorBoundary } from "./components/ErrorBoundary";
import "./styles.css";

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <ErrorBoundary>
      <Portal />
    </ErrorBoundary>
  </StrictMode>
);
