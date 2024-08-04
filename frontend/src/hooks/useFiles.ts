import { useCallback, useEffect, useState } from "react";

type File = {
    id: string;
    lastComment: string;
    lastSyncAt: Date;
    magnet: string;
    name: string;
    originalUrl: string;
    torrentUpdatedAt: Date;
};

const BaseUrl = import.meta.env.VITE_API_BASE_URL || window.location.origin;

export const useFiles = () => {
    const [files, setFiles] = useState<File[]>([]);
    const [loading, setLoading] = useState(true);
    const [error, setError] = useState(null);
    const [refreshing, setRefreshing] = useState(true);

    useEffect(() => {
        if (!refreshing) {
            return;
        }

        fetch(`${BaseUrl}/api/files`)
            .then((response) => response.json())
            .then(setFiles)
            .catch(setError)
            .finally(() => {
                setLoading(false);
                setRefreshing(false);
            });
    }, [refreshing]);

    const onRemove = useCallback(async (id: string) => {
        await fetch(`${BaseUrl}/api/files/${id}`, { method: "DELETE" });
        setRefreshing(true);
    }, []);

    const onRefreshFileMetadata = useCallback(async (id: string) => {
        await fetch(`${BaseUrl}/api/files/${id}/refresh`, { method: "PATCH" });
        setRefreshing(true);
    }, []);

    const onRefreshAllFilesMetadata = useCallback(async () => {
        setRefreshing(true);
        await fetch(`${BaseUrl}/api/files/refresh`, { method: "PATCH" });
        setRefreshing(true);
    }, []);

    return { error, files, loading, onRefreshAllFilesMetadata, onRefreshFileMetadata, onRemove };
};
