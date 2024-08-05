import { SDKProvider, useLaunchParams } from "@telegram-apps/sdk-react";

import { App } from "./App.tsx";
import { ErrorBoundary } from "./ErrorBoundary.tsx";

type ErrorBoundaryErrorProps = {
    error: unknown;
};

const ErrorBoundaryError = ({ error }: ErrorBoundaryErrorProps) => (
    <div>
        <p>An unhandled error occurred:</p>
        <blockquote>
            <code>
                {error instanceof Error ? error.message : typeof error === "string" ? error : JSON.stringify(error)}
            </code>
        </blockquote>
    </div>
);

export const Inner = () => {
    const debug = useLaunchParams().startParam === "debug";

    return (
        <SDKProvider acceptCustomStyles={true} debug={debug}>
            <App />
        </SDKProvider>
    );
};

export const Root = () => (
    <ErrorBoundary fallback={ErrorBoundaryError}>
        <Inner />
    </ErrorBoundary>
);
