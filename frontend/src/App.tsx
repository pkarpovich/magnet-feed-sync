import { AppRoot, Spinner } from "@telegram-apps/telegram-ui";

import styles from "./App.module.css";
import { FileMetadataRow } from "./components/FileMetadataRow.tsx";
import { Header } from "./components/Header.tsx";
import { useFiles } from "./hooks/useFiles.ts";

export const App = () => {
    const { files, loading, onRefreshAllFilesMetadata, onRefreshFileMetadata, onRemove } = useFiles();

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
                        magnet={file.magnet}
                        name={file.name}
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
