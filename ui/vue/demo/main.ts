import { createApp } from "vue";
import "@alixai/vue/styles";
import "./styles/demo.scss";
import App from "./App.vue";

// The demo of @alixai/vue: every component of the kit against the scripted mock backend (ui/demo-mock), served by `pnpm dev`.
createApp(App).mount("#app");
