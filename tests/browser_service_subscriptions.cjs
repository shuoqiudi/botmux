// Run through TestE2E_ServiceSubscriptionBrowser with BROWSER_NODE and
// PUPPETEER_MODULE pointing at the local browser tooling (no npm dependency).
const assert = require('node:assert/strict');
const puppeteer = require(process.env.PUPPETEER_MODULE || 'puppeteer');
(async () => {
 const browser = await puppeteer.launch({headless:true,args:['--no-sandbox']});
 try {
  const page = await browser.newPage();
  await page.setRequestInterception(true);
  page.on('request',r=>r.url().endsWith('/api/version')?r.respond({status:200,contentType:'application/json',body:'{"version":"test","update":{"available":false}}'}):r.continue());
  const errors=[]; page.on('pageerror', e=>errors.push(e.message));
  await page.setViewport({width:1440,height:1400});
  await page.setCookie({name:process.env.SUBSCRIPTION_COOKIE_NAME,value:process.env.SUBSCRIPTION_SESSION,url:process.env.SUBSCRIPTION_URL});
  await page.goto(process.env.SUBSCRIPTION_URL,{waitUntil:'networkidle0'});
  await page.evaluate(()=>{localStorage.setItem('theme','dark');document.documentElement.setAttribute('data-theme','dark');});
  await page.click('#notificationServicesBtn');
  await page.waitForSelector('#notificationServicesList button');
  await page.click('#notificationServicesList button');
  await page.waitForSelector('#serviceSubscriptionForm',{timeout:3000});
  await page.select('#serviceSubscriptionBot',process.env.SUBSCRIPTION_ACCOUNT);
  await page.select('#serviceSubscriptionDestination',process.env.SUBSCRIPTION_DESTINATION);
  await page.$eval('#serviceSubscriptionForm', form=>form.requestSubmit());
  await page.waitForSelector('.service-subscription-cancel');
  const receipt=await page.evaluate(async()=>{
   const r=await fetch('/api/v1/services/notifications',{method:'POST',headers:{'Content-Type':'application/json','Authorization':'Bearer subscriber-secret','Idempotency-Key':'browser-event'},body:JSON.stringify({fingerprint:'dns',text:'<b>Browser DNS</b>',parse_mode:'HTML'})});
   if(r.status!==202)throw Error('Publish '+r.status);return r.json();
  });
  await page.click('#serviceDetailRefresh');
  await page.waitForFunction(()=>document.querySelector('#notificationServiceDetail').textContent.includes('Waiting for delivery'));
  await fetch(process.env.SUBSCRIPTION_TELEGRAM+'/bot940001:subscription-secret/release',{method:'POST'});
  await page.waitForFunction(async id=>{
   const r=await fetch('/api/v1/services/notifications/'+id,{headers:{Authorization:'Bearer subscriber-secret'}});
   return (await r.json()).delivery_summary.status==='succeeded';
  },{},receipt.notification_id);
  await page.click('#serviceDetailRefresh');
  await page.waitForFunction(()=>document.querySelector('#notificationServiceDetail').textContent.includes('All delivered'));
  assert.equal(await page.$('#notificationServiceDetail pre b'),null);
  assert((await page.$eval('#notificationServiceDetail',e=>e.textContent)).includes(receipt.deliveries[0].delivery_id));
  await page.click('.service-subscription-cancel');
  await page.waitForFunction(()=>document.querySelectorAll('.service-subscription-cancel').length===0);
  await page.click('#serviceTargetConfig summary');
  await page.select('#serviceDestinationEdit',process.env.SUBSCRIPTION_DESTINATION);
  await page.$eval('#serviceTargetChat',e=>e.value='-100940002');
  await page.$eval('#serviceTargetForm',form=>form.requestSubmit());
  await page.waitForFunction(()=>document.querySelector('#serviceConfigResult').textContent==='Configuration validated and saved.');
  await page.select('#serviceSubscriptionBot',process.env.SUBSCRIPTION_ACCOUNT);
  await page.select('#serviceSubscriptionDestination',process.env.SUBSCRIPTION_DESTINATION);
  await page.$eval('#serviceSubscriptionForm',form=>form.requestSubmit());
  await page.waitForSelector('.service-subscription-cancel');
  // Create and validate a new Bot and chat using the actual configuration forms.
  await page.click('.service-subscription-cancel');
  await page.waitForFunction(()=>document.querySelectorAll('.service-subscription-cancel').length===0);
  await page.click('#serviceTargetConfig summary');
  await page.click('#serviceAddBot');
  await page.waitForSelector('#botModal',{visible:true});
  await page.type('#botFormName','Browser Bot');
  await page.type('#botFormToken','940003:browser-secret');
  await page.evaluate(()=>saveBot());
  await page.waitForFunction(()=>serviceAccounts.some(a=>a.name==='Browser Bot'));
  await page.click('#serviceTargetConfig summary');
  const newAccount=await page.evaluate(()=>String(serviceAccounts.find(a=>a.name==='Browser Bot').id));
  await page.select('#serviceTargetBot',newAccount);
  await page.type('#serviceTargetName','New browser group');
  await page.type('#serviceTargetChat','-100940003');
  await page.$eval('#serviceTargetForm',form=>form.requestSubmit());
  await page.waitForFunction(()=>serviceDestinations.some(d=>d.name==='New browser group'));
  const newDestination=await page.evaluate(()=>String(serviceDestinations.find(d=>d.name==='New browser group').id));
  await page.select('#serviceSubscriptionBot',newAccount);
  await page.select('#serviceSubscriptionDestination',newDestination);
  await page.$eval('#serviceSubscriptionForm',form=>form.requestSubmit());
  await page.waitForSelector('.service-subscription-cancel');
  // Keep the new Bot and add a second Bot/chat through the same form.
  await page.select('#serviceSubscriptionBot',process.env.SUBSCRIPTION_ACCOUNT);
  await page.select('#serviceSubscriptionDestination',process.env.SUBSCRIPTION_DESTINATION);
  await page.$eval('#serviceSubscriptionForm',form=>form.requestSubmit());
  await page.waitForFunction(()=>document.querySelectorAll('.service-subscription-cancel').length===2);
  const failed=await page.evaluate(async()=>{
    const r=await fetch('/api/v1/services/notifications',{method:'POST',headers:{'Content-Type':'application/json',Authorization:'Bearer subscriber-secret','Idempotency-Key':'browser-failure'},body:JSON.stringify({fingerprint:'dns',text:'Browser failure'})});return r.json();
  });
  await page.waitForFunction(async id=>{
    const r=await fetch('/api/v1/services/notifications/'+id,{headers:{Authorization:'Bearer subscriber-secret'}});return (await r.json()).delivery_summary.status==='failed';
  },{},failed.notification_id);
  assert.equal(failed.subscription_count,2);
  await page.waitForFunction(async id=>{
    const r=await fetch('/api/v1/services/notifications/'+id,{headers:{Authorization:'Bearer subscriber-secret'}});return (await r.json()).delivery_summary.succeeded===1;
  },{},failed.notification_id);
  await page.click('#serviceDetailRefresh');
  await page.waitForFunction(()=>document.querySelector('#notificationServiceDetail').textContent.includes('Failed — dead-letter queue'));
  const history=await page.$eval('#notificationServiceDetail',e=>e.textContent);
  assert(history.includes('940003') && history.includes('-100940003'));
  assert(history.includes('940001') && history.includes('-100940002'));
  assert(history.includes('Delivered targets: 1 / 2'));
  // Cancel a subscription without removing either historical Delivery.
  await page.click('.service-subscription-cancel');
  await page.waitForFunction(()=>document.querySelectorAll('.service-subscription-cancel').length===1);
  for(const d of failed.deliveries) assert(await page.$(`[data-delivery-id="${d.delivery_id}"]`));
  if(process.env.SUBSCRIPTION_SCREENSHOTS){
   await page.screenshot({path:process.env.SUBSCRIPTION_SCREENSHOTS+'/service-subscriptions-en-dark.png',fullPage:true});
  }
  await page.evaluate(()=>{localStorage.setItem('lang','ru');currentLang='ru';applyLang();document.documentElement.setAttribute('data-theme','light');});
  assert((await page.$eval('#notificationServiceDetail',e=>e.textContent)).includes('Подписки'));
  if(process.env.SUBSCRIPTION_SCREENSHOTS){await page.screenshot({path:process.env.SUBSCRIPTION_SCREENSHOTS+'/service-subscriptions-ru-light.png',fullPage:true});}
  // The per-target operation uses the existing audited DLQ endpoint.
  page.on('dialog',dialog=>dialog.accept());
  const failedID=await page.evaluate(id=>notificationServiceDetail.notifications.find(n=>n.notification_id===id).deliveries.find(d=>d.status==='dead-lettered').delivery_id,failed.notification_id);
  await page.click(`[data-delivery-id="${failedID}"] button:last-child`);
  await page.waitForFunction(id=>notificationServiceDetail.notifications.find(n=>n.notification_id===id).deliveries.some(d=>d.status==='discarded'),{},failed.notification_id);
  const audit=await page.evaluate(async()=>{const r=await fetch('/api/gateway/v1/audit');return r.text()});
  assert(audit.includes('dlq.discard') && audit.includes(failedID));
  assert.deepEqual(errors,[]);
  console.log('Browser two-target subscription, partial failure, historical targets, cancellation, audited discard and EN/RU checks passed');
 } finally {await browser.close();}
})().catch(e=>{console.error(e);process.exit(1);});
