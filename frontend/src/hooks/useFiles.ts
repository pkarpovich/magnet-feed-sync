import { useCallback, useEffect, useState } from "react";

import { BaseUrl } from "../constants/api.ts";
import { uniqId } from "../utils/uniqId.ts";

type File = {
    id: string;
    lastComment: string;
    lastSyncAt: Date;
    location: string;
    magnet: string;
    name: string;
    originalUrl: string;
    torrentUpdatedAt: Date;
};

export const useFiles = () => {
    const [files, setFiles] = useState<File[]>([]);
    const [loading, setLoading] = useState(true);
    const [error, setError] = useState(null);
    const [refreshing, setRefreshing] = useState<string>();

    useEffect(() => {
        const controller = new AbortController();
        const signal = controller.signal;

        fetch(`${BaseUrl}/api/files`, { signal })
            .then((response) => {
                if (!response.ok) {
                    throw new Error("Network response was not ok");
                }

                return response.json();
            })
            .then(setFiles)
            .catch(setError)
            .finally(() => {
                setLoading(false);
            });

        return () => controller.abort();
    }, [refreshing]);

    const onRemove = useCallback(async (id: string) => {
        await fetch(`${BaseUrl}/api/files/${id}`, { method: "DELETE" });
        setRefreshing(uniqId());
    }, []);

    const onRefreshFileMetadata = useCallback(async (id: string) => {
        await fetch(`${BaseUrl}/api/files/${id}/refresh`, { method: "PATCH" });
        setRefreshing(uniqId());
    }, []);

    const onRefreshAllFilesMetadata = useCallback(async () => {
        await fetch(`${BaseUrl}/api/files/refresh`, { method: "PATCH" });
        setRefreshing(uniqId());
    }, []);

    const onReloadFiles = useCallback(() => {
        setRefreshing(uniqId());
    }, []);

    return { error, files, loading, onRefreshAllFilesMetadata, onRefreshFileMetadata, onReloadFiles, onRemove };
};
