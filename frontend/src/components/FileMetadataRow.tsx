import { IconButton } from "@telegram-apps/telegram-ui";
import clsx from "clsx";
import { useCallback, useState } from "react";

import MagnetIcon from "../icons/magnet.svg";
import RefreshIcon from "../icons/refresh.svg";
import RemoveIcon from "../icons/remove.svg";
import ShareIcon from "../icons/share.svg";
import styles from "./FileMetadataRow.module.css";

type Props = {
    id: string;
    lastComment: string;
    lastSyncAt: Date;
    magnet: string;
    name: string;
    onRefreshFileMetadata: (id: string) => Promise<void>;
    onRemove: (id: string) => Promise<void>;
    originalUrl: string;
    torrentUpdatedAt: Date;
};

export const FileMetadataRow = ({
    id,
    lastComment,
    lastSyncAt,
    magnet,
    name,
    onRefreshFileMetadata,
    onRemove,
    originalUrl,
    torrentUpdatedAt,
}: Props) => {
    const [isRefreshing, setIsRefreshing] = useState(false);

    const handleMagnetClick = useCallback(async () => {
        await navigator.clipboard.writeText(magnet);
    }, [magnet]);

    const handleShareClick = useCallback(() => {
        window.open(originalUrl, "_blank");
    }, [originalUrl]);

    const handleRefreshClick = useCallback(async () => {
        setIsRefreshing(true);
        await onRefreshFileMetadata(id);
        setIsRefreshing(false);
    }, [id, onRefreshFileMetadata]);

    const handleRemoveClick = useCallback(async () => {
        await onRemove(id);
    }, [id, onRemove]);

    return (
        <div className={styles.row}>
            <div className={styles.header}>
                <div className={styles.headerUtil}>
                    <span className={styles.date}>Updated: {new Date(torrentUpdatedAt).toLocaleDateString()}</span>
                    <div className={styles.headerIcons}>
                        <IconButton className={styles.headerIcon} mode="bezeled" onClick={handleMagnetClick} size="s">
                            <MagnetIcon />
                        </IconButton>
                        <IconButton className={styles.headerIcon} mode="bezeled" onClick={handleShareClick} size="s">
                            <ShareIcon />
                        </IconButton>
                        <IconButton
                            className={clsx(styles.headerIcon, {
                                [styles.headerIconActive]: isRefreshing,
                            })}
                            mode="bezeled"
                            onClick={handleRefreshClick}
                            size="s"
                            title={new Date(lastSyncAt).toLocaleString()}
                        >
                            <RefreshIcon />
                        </IconButton>
                        <IconButton className={styles.headerIcon} mode="bezeled" onClick={handleRemoveClick} size="s">
                            <RemoveIcon />
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
