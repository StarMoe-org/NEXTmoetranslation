import test from "node:test";
import assert from "node:assert/strict";
import { mysekaiTemplate, parseMysekaiTemplate } from "../src/lib/mysekai-txt.mjs";
const hash = "a".repeat(64);
const detail = {region:"jp", locale:"zh-CN",sourceRevision:hash,script:{key:"synthetic",lines:[{id:"line-a",kind:"text",sourceHash:hash,text:"Original"}]},related:[],drafts:{}};
test("TXT import keeps identity, newline and literal markup as data", () => {
 const text = mysekaiTemplate(detail).replace("<<<译文\n\n", "<<<译文\n翻译\n<script>literal</script>\n");
 const result = parseMysekaiTemplate(text,detail);
 assert.equal(result[0].id,"line-a");assert.equal(result[0].text,"翻译\n<script>literal</script>");assert.equal(result[0].conflict,"");
});
test("empty translations cannot erase prior work; stale source and edit revisions need review",()=>{
 assert.deepEqual(parseMysekaiTemplate(mysekaiTemplate(detail),detail),[]);
 const text=mysekaiTemplate(detail).replace("<<<译文\n\n","<<<译文\nTranslation\n");
 const current={...detail,drafts:{"line-a":{revision:1,text:"New",sourceHash:hash}}};
 assert.match(parseMysekaiTemplate(text,current)[0].conflict,/更新的译文/);
 const changed={...detail,script:{...detail.script,lines:[{...detail.script.lines[0],sourceHash:"b".repeat(64)}]}};
 assert.match(parseMysekaiTemplate(text,changed)[0].conflict,/原文有更新/);
 assert.throws(()=>parseMysekaiTemplate(text,{...detail,locale:"en-US"}),/语言/);
});
