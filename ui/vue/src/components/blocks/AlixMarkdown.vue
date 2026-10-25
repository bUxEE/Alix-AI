<script setup lang="ts">
import { computed } from "vue";
import { parseMarkdown } from "../../markdown";

// Assistant text, rendered as text. The Markdown is parsed into a closed tree (see markdown.ts) and every node is drawn by this
// template: there is no HTML path, so nothing in the text can become markup. Links keep an address only when it is http, https or
// mailto; set `links` to false when the host wants to own routing and show link text only.

const props = withDefaults(
  defineProps<{
    source: string;
    links?: boolean;
  }>(),
  { links: true },
);

const blocks = computed(() => parseMarkdown(props.source, props.links));
</script>

<template>
  <div class="alix-prose">
    <template
      v-for="(block, index) in blocks"
      :key="index"
    >
      <p v-if="block.kind === 'paragraph'">
        <template
          v-for="(part, at) in block.inline"
          :key="at"
        >
          <a
            v-if="part.kind === 'link'"
            :href="part.href"
            rel="noopener noreferrer nofollow"
            target="_blank"
          >
            {{ part.text }}
          </a>
          <code v-else-if="part.kind === 'code'">
            {{ part.text }}
          </code>
          <strong v-else-if="part.kind === 'bold'">
            {{ part.text }}
          </strong>
          <em v-else-if="part.kind === 'italic'">
            {{ part.text }}
          </em>
          <template v-else>
            {{ part.text }}
          </template>
        </template>
      </p>
      <component
        v-else-if="block.kind === 'heading'"
        :is="`h${block.level + 1}`"
      >
        <template
          v-for="(part, at) in block.inline"
          :key="at"
        >
          <code v-if="part.kind === 'code'">
            {{ part.text }}
          </code>
          <template v-else>
            {{ part.text }}
          </template>
        </template>
      </component>
      <!-- One line on purpose: whitespace inside <pre> is content. -->
      <pre v-else-if="block.kind === 'code'"><code :data-language="block.language || undefined">{{ block.text }}</code></pre>
      <blockquote v-else-if="block.kind === 'quote'">
        <template
          v-for="(part, at) in block.inline"
          :key="at"
        >
          {{ part.text }}
        </template>
      </blockquote>
      <component
        v-else-if="block.kind === 'list'"
        :is="block.ordered ? 'ol' : 'ul'"
      >
        <li
          v-for="(item, at) in block.items"
          :key="at"
        >
          <template
            v-for="(part, n) in item"
            :key="n"
          >
            <a
              v-if="part.kind === 'link'"
              :href="part.href"
              rel="noopener noreferrer nofollow"
              target="_blank"
            >
              {{ part.text }}
            </a>
            <code v-else-if="part.kind === 'code'">
              {{ part.text }}
            </code>
            <strong v-else-if="part.kind === 'bold'">
              {{ part.text }}
            </strong>
            <em v-else-if="part.kind === 'italic'">
              {{ part.text }}
            </em>
            <template v-else>
              {{ part.text }}
            </template>
          </template>
        </li>
      </component>
      <hr v-else-if="block.kind === 'rule'">
    </template>
  </div>
</template>
