import { useCallback, useEffect, useRef, useState } from "react";

/*
 * R4.54：媒体预览加载排队（方案 A/C）。
 *
 * 背景（2026-09-25 实测）：浏览器对同一 HTTP/1.1 源最多 6 条并发连接，
 * 聊天页的图片预览/缩略图/头像都是长连接流，几张慢图就能占满全部 socket，
 * 「加载更早消息」等 API 请求在浏览器内部排队到 80s 超时——后端其实空闲。
 *
 * 方案：预览类 <img> 统一经 useMediaLoadSlot 拿「加载名额」（并发上限 3），
 * 拿到名额才真正设置 src 发起请求；API fetch 不排队（并在 api() 里默认
 * priority: high），从根上保证会话加载优先于预览图。
 *
 * R4.56 修正（2.6.32 实测事故：会话列表头像大面积裂图）：名额原先只在
 * 组件卸载时释放，而会话列表几百行全部常驻挂载——前三张图加载完就把
 * 名额永久占住，其余头像永远排队（src 为空 → 裂图 + alt 文案）。
 * 现改为「加载完成/失败即释放名额」，卸载仅作兜底；名额只覆盖「请求在途」
 * 窗口，加载完的图继续正常显示（浏览器缓存接管）。
 */

const MAX_CONCURRENT_MEDIA = 3;

let activeCount = 0;
const waiters = [];

function pump() {
  while (activeCount < MAX_CONCURRENT_MEDIA && waiters.length > 0) {
    const waiter = waiters.shift();
    // 名额在发放瞬间记账（而非等 granted effect），杜绝「fire 后、渲染前卸载」漏计。
    activeCount += 1;
    waiter.fire();
  }
}

// 返回 [是否已拿到名额, 释放名额]。图片 onLoad/onError 后必须释放，
// 否则常驻列表会把并发名额占死（R4.56 事故根因）。
export function useMediaLoadSlot() {
  const [granted, setGranted] = useState(false);
  const grantedRef = useRef(false);
  const releasedRef = useRef(false);

  const release = useCallback(() => {
    if (releasedRef.current) return;
    releasedRef.current = true;
    activeCount = Math.max(0, activeCount - 1);
    pump();
  }, []);

  // 真实卸载时若名额在手且未释放 → 释放（兜底：加载中卸载/报错路径漏调）。
  useEffect(() => {
    return () => {
      if (grantedRef.current) release();
    };
  }, [release]);

  useEffect(() => {
    if (granted) return; // 名额已在 pump() 发放时记账，此处无需重复
    const waiter = {
      cancelled: false,
      fire: () => {
        if (waiter.cancelled) return;
        grantedRef.current = true;
        setGranted(true);
      }
    };
    waiters.push(waiter);
    pump();
    return () => {
      waiter.cancelled = true;
      const index = waiters.indexOf(waiter);
      if (index >= 0) waiters.splice(index, 1);
      // fire 后、granted 渲染前卸载的场景由上方 unmount cleanup 兜住（grantedRef 已置位）。
    };
  }, [granted]);

  return [granted, release];
}

// 排队加载的预览图：拿到名额前不设 src（不发请求），拿到后才真正加载；
// 加载完成/失败立即释放名额。其余 props（className/style/onError 等）透传。
export function QueuedImage({ src, alt = "", ...rest }) {
  const [granted, release] = useMediaLoadSlot();
  const imgRef = useRef(null);
  const settledRef = useRef(false);

  const settle = useCallback(() => {
    if (settledRef.current) return;
    settledRef.current = true;
    release();
  }, [release]);

  // 同一实例换 src（少见：行复用/账号切换）：重新走一遍「在途」窗口。
  useEffect(() => {
    settledRef.current = false;
  }, [src]);

  // loading="lazy" 的图在滚近视口前不会发起请求：拿到名额但 currentSrc 为空
  // 说明名额被「还没轮到加载」的图占着——主动让出，滚近时浏览器自行加载。
  useEffect(() => {
    if (!granted) return undefined;
    const img = imgRef.current;
    if (img && img.complete) {
      settle();
      return undefined;
    }
    const timer = setTimeout(() => {
      const current = imgRef.current;
      if (current && !current.currentSrc) settle();
    }, 1200);
    return () => clearTimeout(timer);
  }, [granted, src, settle]);

  const { onError, onLoad, ...others } = rest;
  return (
    <img
      ref={imgRef}
      {...others}
      src={granted ? src : undefined}
      alt={alt}
      loading="lazy"
      fetchpriority="low"
      onLoad={(event) => { settle(); if (onLoad) onLoad(event); }}
      onError={(event) => { settle(); if (onError) onError(event); }}
    />
  );
}
