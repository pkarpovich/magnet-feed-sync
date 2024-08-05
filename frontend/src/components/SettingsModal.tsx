import { Divider, Headline, IconButton, Modal, Select } from "@telegram-apps/telegram-ui";
import type { ChangeEvent } from "react";
import { useCallback } from "react";

import type { FileLocation } from "../hooks/useFileLocations.ts";
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
    const handleChange = useCallback(
        async (e: ChangeEvent<HTMLSelectElement>) => {
            await onChange(id, e.target.value);
        },
        [id, onChange],
    );

    return (
        <Modal
            className={styles.settingsContainer}
            header={<SettingsModalHeader>Settings</SettingsModalHeader>}
            trigger={
                <div>
                    <IconButton className={styles.headerIcon} mode="bezeled" size="s">
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
