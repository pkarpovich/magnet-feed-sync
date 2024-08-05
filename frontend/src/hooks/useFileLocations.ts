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
        const controller = new AbortController();
        const signal = controller.signal;

        fetch(`${BaseUrl}/api/file-locations`, { signal })
            .then((response) => {
                if (!response.ok) {
                    throw new Error("Network response was not ok");
                }

                return response.json();
            })
            .then(setLocations)
            .catch(setError)
            .finally(() => setLoading(false));

        return () => controller.abort();
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
