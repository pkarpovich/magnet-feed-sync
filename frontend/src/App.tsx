import { AppRoot } from "@telegram-apps/telegram-ui";

import { FileMetadataRow } from "./components/FileMetadataRow.tsx";
import { useFiles } from "./hooks/useFiles.ts";

export const App = () => {
    const { files } = useFiles();

    return (
        <AppRoot>
            {files.map((file) => (
                <FileMetadataRow
                    id={file.id}
                    key={file.id}
                    lastComment={file.lastComment}
                    lastSyncAt={file.lastSyncAt}
                    magnet={file.magnet}
                    name={file.name}
                    originalUrl={file.originalUrl}
                    torrentUpdatedAt={file.torrentUpdatedAt}
                />
            ))}
        </AppRoot>
    );
};
