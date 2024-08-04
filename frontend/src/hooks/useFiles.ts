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

export const useFiles = () => {
    const [files, setFiles] = useState<File[]>([]);
    const [loading, setLoading] = useState(true);
    const [error, setError] = useState(null);

    useEffect(() => {
        fetch("http://localhost:8080/files")
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
