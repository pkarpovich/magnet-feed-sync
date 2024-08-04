import { IconButton } from "@telegram-apps/telegram-ui";
import { useCallback } from "react";

import MagnetIcon from "../icons/magnet.svg";
import RefreshIcon from "../icons/refresh.svg";
import ShareIcon from "../icons/share.svg";
import styles from "./FileMetadataRow.module.css";

type Props = {
    id: string;
    lastComment: string;
    lastSyncAt: Date;
    magnet: string;
    name: string;
    originalUrl: string;
    torrentUpdatedAt: Date;
};

export const FileMetadataRow = ({ lastComment, lastSyncAt, magnet, name, originalUrl, torrentUpdatedAt }: Props) => {
    const handleMagnetClick = useCallback(async () => {
        await navigator.clipboard.writeText(magnet);
    }, [magnet]);

    const handleShareClick = useCallback(() => {
        window.open(originalUrl, "_blank");
    }, [originalUrl]);

    const handleRefreshClick = useCallback(async () => {
        console.log("Refresh clicked");
    }, []);

    return (
        <div className={styles.row}>
            <div className={styles.header}>
                <div className={styles.headerUtil}>
                    <span className={styles.date}>Updated: {new Date(torrentUpdatedAt).toLocaleDateString()}</span>
                    <div className={styles.headerIcons}>
                        <IconButton mode="bezeled" onClick={handleMagnetClick} size="s">
                            <MagnetIcon />
                        </IconButton>
                        <IconButton mode="bezeled" onClick={handleShareClick} size="s">
                            <ShareIcon />
                        </IconButton>
                        <IconButton
                            mode="bezeled"
                            onClick={handleRefreshClick}
                            size="s"
                            title={new Date(lastSyncAt).toLocaleString()}
                        >
                            <RefreshIcon />
                        </IconButton>
                    </div>
                </div>
                <a className={styles.name} href={originalUrl} rel="noopener noreferrer" target="_blank">
                    {name}
                </a>
            </div>
            <div className={styles.content}>
                <div className={styles.field}>
                    <strong>Last Comment:</strong> {lastComment ? lastComment : "No comments"}
                </div>
            </div>
        </div>
    );
};
