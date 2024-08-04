import { useEffect, useState } from "react";

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

    useEffect(() => {
        fetch(`${BaseUrl}/api/files`)
            .then((response) => response.json())
            .then((data) => {
                setFiles(data);
                setLoading(false);
            })
            .catch((error) => {
                setError(error);
                setLoading(false);
            });
    }, []);

    return { error, files, loading };
};
