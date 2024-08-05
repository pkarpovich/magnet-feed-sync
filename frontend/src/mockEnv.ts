import { mockTelegramEnv, parseInitData, retrieveLaunchParams } from "@telegram-apps/sdk-react";

const urlParams = new URLSearchParams(window.location.search);
const debug = urlParams.get("debug") === "true";

// It is important, to mock the environment only for development purposes. When building the
// application, import.meta.env.DEV will become false, and the code inside will be tree-shaken,
// so you will not see it in your final bundle.
if (import.meta.env.DEV || debug) {
    let shouldMock: boolean;

    // Try to extract launch parameters to check if the current environment is Telegram-based.
    try {
        // If we are able to extract launch parameters, it means that we are already in the
        // Telegram environment. So, there is no need to mock it.
        retrieveLaunchParams();

        // We could previously mock the environment. In case we did, we should do it again. The reason
        // is the page could be reloaded, and we should apply mock again, because mocking also
        // enables modifying the window object.
        shouldMock = Boolean(sessionStorage.getItem("____mocked"));
    } catch (_) {
        shouldMock = true;
    }

    if (shouldMock) {
        const initDataRaw = new URLSearchParams([
            [
                "user",
                JSON.stringify({
                    allows_write_to_pm: true,
                    first_name: "Andrew",
                    id: 99281932,
                    is_premium: true,
                    language_code: "en",
                    last_name: "Rogue",
                    username: "rogue",
                }),
            ],
            ["hash", "89d6079ad6762351f38c6dbbc41bb53048019256a9443988af7a48bcad16ba31"],
            ["auth_date", "1716922846"],
            ["start_param", "debug"],
            ["chat_type", "sender"],
            ["chat_instance", "8428209589180549439"],
        ]).toString();

        mockTelegramEnv({
            initData: parseInitData(initDataRaw),
            initDataRaw,
            platform: "tdesktop",
            themeParams: {
                accentTextColor: "#6ab2f2",
                bgColor: "#17212b",
                buttonColor: "#5288c1",
                buttonTextColor: "#ffffff",
                destructiveTextColor: "#ec3942",
                headerBgColor: "#17212b",
                hintColor: "#708499",
                linkColor: "#6ab3f3",
                secondaryBgColor: "#232e3c",
                sectionBgColor: "#17212b",
                sectionHeaderTextColor: "#6ab3f3",
                subtitleTextColor: "#708499",
                textColor: "#f5f5f5",
            },
            version: "7.2",
        });
        sessionStorage.setItem("____mocked", "1");

        console.info(
            "As long as the current environment was not considered as the Telegram-based one, it was mocked. Take a note, that you should not do it in production and current behavior is only specific to the development process. Environment mocking is also applied only in development mode. So, after building the application, you will not see this behavior and related warning, leading to crashing the application outside Telegram.",
        );
    }
}
