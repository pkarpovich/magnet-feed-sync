import { IconButton } from "@telegram-apps/telegram-ui";
import clsx from "clsx";
import { useCallback, useState } from "react";

import HardRefreshIcon from "../icons/hard-refresh.svg";
import styles from "./Header.module.css";
import { Logo } from "./Logo.tsx";

type Props = {
    onRefresh: () => Promise<void>;
};

export const Header = ({ onRefresh }: Props) => {
    const [isRefreshing, setIsRefreshing] = useState(false);

    const handleRefreshClick = useCallback(async () => {
        setIsRefreshing(true);
        await onRefresh();
        setIsRefreshing(false);
    }, [onRefresh]);

    return (
        <header className={styles.headerContainer}>
            <div className={styles.headerLogo}>
                <Logo />
            </div>
            <span className={styles.headerTitle}>Magnet Feed Sync</span>
            <div className={styles.headerActions}>
                <IconButton
                    className={clsx(styles.headerIcon, {
                        [styles.headerIconActive]: isRefreshing,
                    })}
                    mode="bezeled"
                    onClick={handleRefreshClick}
                    size="s"
                >
                    <HardRefreshIcon />
                </IconButton>
            </div>
        </header>
    );
};
