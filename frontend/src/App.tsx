import { AppRoot, Spinner } from "@telegram-apps/telegram-ui";
import { useCallback } from "react";

import styles from "./App.module.css";
import { FileMetadataRow } from "./components/FileMetadataRow.tsx";
import { Header } from "./components/Header.tsx";
import { useFileLocations } from "./hooks/useFileLocations.ts";
import { useFiles } from "./hooks/useFiles.ts";

export const App = () => {
    const { files, loading, onRefreshAllFilesMetadata, onRefreshFileMetadata, onReloadFiles, onRemove } = useFiles();
    const { locations, onUpdateFileLocation } = useFileLocations();

    const handleUpdateFileLocation = useCallback(
        async (fileId: string, newLocation: string) => {
            await onUpdateFileLocation(fileId, newLocation);
            onReloadFiles();
        },
        [onReloadFiles, onUpdateFileLocation],
    );

    return (
        <AppRoot>
            <Header onRefresh={onRefreshAllFilesMetadata} />
            {loading ? (
                <div className={styles.loadingContainer}>
                    <Spinner size="l" />
                </div>
            ) : null}
            <div className={styles.container}>
                {files.map((file) => (
                    <FileMetadataRow
                        id={file.id}
                        key={file.id}
                        lastComment={file.lastComment}
                        lastSyncAt={file.lastSyncAt}
                        location={file.location}
                        locations={locations}
                        magnet={file.magnet}
                        name={file.name}
                        onLocationChange={handleUpdateFileLocation}
                        onRefreshFileMetadata={onRefreshFileMetadata}
                        onRemove={onRemove}
                        originalUrl={file.originalUrl}
                        torrentUpdatedAt={file.torrentUpdatedAt}
                    />
                ))}
            </div>
        </AppRoot>
    );
};
