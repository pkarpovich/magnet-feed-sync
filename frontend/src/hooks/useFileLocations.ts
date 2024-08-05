import { useCallback, useEffect, useState } from "react";

import { BaseUrl } from "../constants/api.ts";

export type FileLocation = {
    id: string;
    name: string;
};

export const useFileLocations = () => {
    const [locations, setLocations] = useState<FileLocation[]>([]);
    const [loading, setLoading] = useState(true);
    const [error, setError] = useState(null);

    useEffect(() => {
        fetch(`${BaseUrl}/api/file-locations`)
            .then((response) => response.json())
            .then(setLocations)
            .catch(setError)
            .finally(() => setLoading(false));
    }, []);

    const onUpdateFileLocation = useCallback(async (fileId: string, newLocation: string) => {
        await fetch(`${BaseUrl}/api/file-locations`, {
            body: JSON.stringify({ fileId, location: newLocation }),
            headers: {
                "Content-Type": "application/json",
            },
            method: "POST",
        });
    }, []);

    return { error, loading, locations, onUpdateFileLocation };
};
