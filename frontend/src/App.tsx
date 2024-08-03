import "./App.css";

import { SDKProvider, useLaunchParams } from "@telegram-apps/sdk-react";
import { useEffect } from "react";

export const App = () => {
    const debug = useLaunchParams().startParam === "debug";

    useEffect(() => {
        if (debug) {
            import("eruda").then((lib) => lib.default.init());
        }
    }, [debug]);

    return (
        <SDKProvider acceptCustomStyles={true} debug={debug}>
            <div>magnet-feed-sync</div>
        </SDKProvider>
    );
};
