import {
    type ComponentType,
    type GetDerivedStateFromError,
    type PropsWithChildren,
    PureComponent,
    type ReactNode,
} from "react";

export type ErrorBoundaryProps = {
    fallback?: ComponentType<{ error: unknown }> | ReactNode;
} & PropsWithChildren;

type ErrorBoundaryState = {
    error?: unknown;
};

export class ErrorBoundary extends PureComponent<ErrorBoundaryProps, ErrorBoundaryState> {
    static getDerivedStateFromError: GetDerivedStateFromError<ErrorBoundaryProps, ErrorBoundaryState> = (error) => ({
        error,
    });

    state: ErrorBoundaryState = {};

    componentDidCatch(error: Error) {
        this.setState({ error });
    }

    render() {
        const {
            props: { children, fallback: Fallback },
            state: { error },
        } = this;

        return "error" in this.state ? (
            typeof Fallback === "function" ? (
                <Fallback error={error} />
            ) : (
                Fallback
            )
        ) : (
            children
        );
    }
}
