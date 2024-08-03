import "./index.css";
import "@telegram-apps/telegram-ui/dist/styles.css";
import "./mockEnv.ts";

import React from "react";
import ReactDOM from "react-dom/client";

import { Root } from "./Root.tsx";

ReactDOM.createRoot(document.getElementById("root")!).render(
    <React.StrictMode>
        <Root />
    </React.StrictMode>,
);
