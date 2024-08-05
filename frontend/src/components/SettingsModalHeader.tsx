import { Text } from "@telegram-apps/telegram-ui";
import clsx from "clsx";
import { forwardRef, HTMLAttributes, ReactNode } from "react";

import styles from "./SettingsModalHeader.module.css";

type ModalHeaderProps = {
    after?: ReactNode;
    before?: ReactNode;
} & HTMLAttributes<HTMLElement>;

export const SettingsModalHeader = forwardRef<HTMLElement, ModalHeaderProps>(
    ({ after, before, children, className, ...props }, ref) => (
        <header className={clsx(styles.wrapper, className)} ref={ref} {...props}>
            <div className={styles.before}>{before}</div>
            <Text className={styles.children} weight="2">
                {children}
            </Text>
            <div className={styles.after}>{after}</div>
        </header>
    ),
);
