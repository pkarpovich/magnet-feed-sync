import { Divider, Headline, IconButton, Modal, Select } from "@telegram-apps/telegram-ui";
import type { ChangeEvent } from "react";
import { useCallback, useState } from "react";

import type { FileLocation } from "../hooks/useFileLocations.ts";
import CloseIcon from "../icons/close.svg";
import SettingsIcon from "../icons/settings.svg";
import styles from "./FileMetadataRow.module.css";
import { SettingsModalHeader } from "./SettingsModalHeader.tsx";

type Props = {
    id: string;
    locations: FileLocation[];
    onChange: (fileId: string, newLocation: string) => Promise<void>;
    title: string;
    value: string;
};

export const SettingsModal = ({ id, locations, onChange, title, value }: Props) => {
    const [isSettingsOpen, setIsSettingsOpen] = useState(false);

    const handleChange = useCallback(
        async (e: ChangeEvent<HTMLSelectElement>) => {
            await onChange(id, e.target.value);
        },
        [id, onChange],
    );

    const handleSettingsClose = useCallback(() => {
        setIsSettingsOpen(false);
    }, []);

    const handleSettingsOpen = useCallback(() => {
        setIsSettingsOpen(true);
    }, []);

    return (
        <Modal
            className={styles.settingsContainer}
            dismissible={false}
            header={
                <SettingsModalHeader
                    after={
                        <IconButton className={styles.headerIcon} mode="plain" onClick={handleSettingsClose} size="m">
                            <CloseIcon />
                        </IconButton>
                    }
                >
                    Settings
                </SettingsModalHeader>
            }
            open={isSettingsOpen}
            trigger={
                <div>
                    <IconButton className={styles.headerIcon} mode="bezeled" onClick={handleSettingsOpen} size="s">
                        <SettingsIcon />
                    </IconButton>
                </div>
            }
        >
            <Headline className={styles.settingsTitle}>{title}</Headline>
            <Divider />
            <Select header="Location" onChange={handleChange} value={value}>
                {locations.map((location) => (
                    <option key={location.id} value={location.id}>
                        {location.name}
                    </option>
                ))}
            </Select>
        </Modal>
    );
};
